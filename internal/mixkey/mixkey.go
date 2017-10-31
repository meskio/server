// mixkey.go - Mix keys and associated utilities.
// Copyright (C) 2017  Yawning Angel.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package mixkey provides persistent mix keys and associated utilities.
package mixkey

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"

	bolt "github.com/coreos/bbolt"
	"github.com/katzenpost/core/crypto/ecdh"
	"github.com/katzenpost/core/crypto/rand"
	"github.com/katzenpost/core/epochtime"
)

const (
	replayBucket   = "replay"
	metadataBucket = "metadata"
)

// MixKey is a Katzenpost server mix key.
type MixKey struct {
	db      *bolt.DB
	keypair *ecdh.PrivateKey
	epoch   uint64

	refCount        int32
	unlinkIfExpired bool
}

// SetUnlinkIfExpired sets if the key will be deleted when closed if it is
// expired.
func (k *MixKey) SetUnlinkIfExpired(b bool) {
	k.unlinkIfExpired = b
}

// PublicKey returns the public component of the key.
func (k *MixKey) PublicKey() *ecdh.PublicKey {
	return k.keypair.PublicKey()
}

// PrivateKey returns the private component of the key.
func (k *MixKey) PrivateKey() *ecdh.PrivateKey {
	return k.keypair
}

// Epoch returns the Katzenpost epoch associated with the keypair.
func (k *MixKey) Epoch() uint64 {
	return k.epoch
}

// IsReplay marks a given replay tag as seen, and returns true iff the tag has
// been seen previously (Test and Set).
func (k *MixKey) IsReplay(tag []byte) bool {
	// Treat all pathologically malformed tags as replays.
	if len(tag) == 0 {
		return true
	}

	// TODO/perf: This probably should do something clever like a bloom filter
	// combined with a write-back cache.

	var seenCount uint64
	if err := k.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(replayBucket))
		if bkt == nil {
			panic("BUG: mixkey: `replay` bucket is missing")
		}

		// Retreive the counter from the database for the tag if it exists.
		//
		// XXX: The counter isn't actually used for anything since it isn't
		// returned.  Not sure if it makes sense to keep it, but I don't think
		// it costs us anything substantial to do so.
		if b := bkt.Get(tag); len(b) == 8 {
			seenCount = binary.LittleEndian.Uint64(b)
		}
		seenCount++         // Increment the counter by 1.
		if seenCount == 0 { // Should never happen ever, but handle correctly.
			seenCount = math.MaxUint64
		}

		// Write the (potentially incremented) counter.
		var seenBytes [8]byte
		binary.LittleEndian.PutUint64(seenBytes[:], seenCount)
		bkt.Put(tag, seenBytes[:])
		return nil
	}); err != nil {
		panic("BUG: mixkey: Failed to query/update the replay counter: " + err.Error())
	}

	return seenCount != 1
}

// Deref reduces the refcount by one, and closes the key if the refcount hits
// 0.
func (k *MixKey) Deref() {
	i := atomic.AddInt32(&k.refCount, -1)
	if i == 0 {
		k.forceClose()
	} else if i < 0 {
		panic("BUG: mixkey: Refcount is negative")
	}
}

// Ref increases the refcount by one.
func (k *MixKey) Ref() {
	i := atomic.AddInt32(&k.refCount, 1)
	if i <= 1 {
		panic("BUG: mixkey: Refcount was 0 or negative")
	}
}

func (k *MixKey) forceClose() {
	if k.db != nil {
		f := k.db.Path() // Cache so we can unlink after Close().

		k.flush()
		k.db.Close()
		k.db = nil

		// Delete the database if the key is expired, and the owner requested
		// full cleanup.
		epoch, _, _ := epochtime.Now()
		if k.unlinkIfExpired && k.epoch < epoch-1 {
			// People will probably complain that this doesn't attempt
			// "secure" deletion, but that's fundementally a lost cause
			// given how many levels of indirection there are to files vs
			// the raw physical media, and the cleanup process being slightly
			// race prone around epoch transitions.  Use FDE.
			os.Remove(f)
		}
	}
	if k.keypair != nil {
		k.keypair.Reset()
		k.keypair = nil
	}
}

func (k *MixKey) flush() error {
	// TODO: When more sophistication is added, flush our cache.
	// See IsReplay() for the planned "more sophistication".

	k.db.Sync()
	return nil
}

// New creates (or loads) a mix key in the provided data directory, for the
// given epoch.
func New(dataDir string, epoch uint64) (*MixKey, error) {
	const (
		versionKey = "version"
		pkKey      = "privateKey"
		epochKey   = "epochKey"
	)
	var err error

	// Initialize the structure and create or open the database.
	f := filepath.Join(dataDir, fmt.Sprintf("mixkey-%d.db", epoch))
	k := new(MixKey)
	k.epoch = epoch
	k.refCount = 1
	k.db, err = bolt.Open(f, 0600, nil) // TODO: O_DIRECT?
	if err != nil {
		return nil, err
	}

	didCreate := false
	if err := k.db.Update(func(tx *bolt.Tx) error {
		// Ensure that all the buckets exist, and grab the metadata bucket.
		bkt, err := tx.CreateBucketIfNotExists([]byte(metadataBucket))
		if err != nil {
			return err
		}
		if _, err = tx.CreateBucketIfNotExists([]byte(replayBucket)); err != nil {
			return err
		}

		if b := bkt.Get([]byte(versionKey)); b != nil {
			// Well, looks like we loaded as opposed to created.
			if len(b) != 1 || b[0] != 0 {
				return fmt.Errorf("mixkey: incompatible version: %d", uint(b[0]))
			}

			// Deserialize the key.
			if b = bkt.Get([]byte(pkKey)); b == nil {
				return fmt.Errorf("mixkey: db missing privateKey entry")
			}
			k.keypair = new(ecdh.PrivateKey)
			if err = k.keypair.FromBytes(b); err != nil {
				return err
			}

			getUint64 := func(key string) (uint64, error) {
				var buf []byte
				if buf = bkt.Get([]byte(key)); buf == nil {
					return 0, fmt.Errorf("mixkey: db missing entry '%v'", key)
				}
				if len(buf) != 8 {
					return 0, fmt.Errorf("mixkey: db corrupted entry '%v'", key)
				}
				return binary.LittleEndian.Uint64(buf), nil
			}

			// Ensure the epoch is sane.
			if dbEpoch, err := getUint64(epochKey); err != nil {
				return err
			} else if dbEpoch != epoch {
				return fmt.Errorf("mixkey: db epoch mismatch")
			}

			return nil
		}

		// If control reaches here, then a new key needs to be created.
		didCreate = true
		k.keypair, err = ecdh.NewKeypair(rand.Reader)
		if err != nil {
			return err
		}
		var epochBytes [8]byte
		binary.LittleEndian.PutUint64(epochBytes[:], epoch)

		// Stash the version/key/epoch in the metadata bucket.
		bkt.Put([]byte(versionKey), []byte{0})
		bkt.Put([]byte(pkKey), k.keypair.Bytes())
		bkt.Put([]byte(epochKey), epochBytes[:])

		return nil
	}); err != nil {
		k.db.Close()
		return nil, err
	}
	if didCreate {
		// Flush the newly created database to disk.
		k.db.Sync()
	}

	return k, nil
}
