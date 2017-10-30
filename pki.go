// pki.go - Katzenpost server PKI interface.
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

package server

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/katzenpost/core/epochtime"
	cpki "github.com/katzenpost/core/pki"
	"github.com/katzenpost/core/sphinx/constants"
	"github.com/katzenpost/core/wire"
	"github.com/op/go-logging"
)

const (
	pkiEarlyConnectSlack = 30 * time.Minute
	pkiLateConnectSlack  = 3 * time.Minute
)

type pkiCacheEntry struct {
	doc      *cpki.Document
	self     *cpki.MixDescriptor
	incoming map[[constants.NodeIDLength]byte]*cpki.MixDescriptor
	outgoing map[[constants.NodeIDLength]byte]*cpki.MixDescriptor
}

func (e *pkiCacheEntry) isOurLayerSane(isProvider bool) bool {
	if isProvider && e.self.Layer != cpki.LayerProvider {
		return false
	}
	if !isProvider {
		if e.self.Layer == cpki.LayerProvider {
			return false
		}
		if int(e.self.Layer) >= len(e.doc.Topology) {
			return false
		}
	}
	return true
}

func (e *pkiCacheEntry) incomingLayer() uint8 {
	switch e.self.Layer {
	case cpki.LayerProvider:
		return uint8(len(e.doc.Topology)) - 1
	case 0:
		return cpki.LayerProvider
	}
	return e.self.Layer - 1
}

func (e *pkiCacheEntry) outgoingLayer() uint8 {
	switch int(e.self.Layer) {
	case len(e.doc.Topology) - 1:
		return cpki.LayerProvider
	case cpki.LayerProvider:
		return 0
	}
	return e.self.Layer + 1
}

func newPKICacheEntry(s *Server, d *cpki.Document) (*pkiCacheEntry, error) {
	e := new(pkiCacheEntry)
	e.doc = d
	e.incoming = make(map[[constants.NodeIDLength]byte]*cpki.MixDescriptor)
	e.outgoing = make(map[[constants.NodeIDLength]byte]*cpki.MixDescriptor)

	// Find our descriptor.
	var err error
	e.self, err = d.GetNodeByKey(s.identityKey.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}

	// And sanity check our descriptor.
	if len(d.Topology) == 0 {
		return nil, fmt.Errorf("pki: document is missing Topology")
	}
	if s.cfg.Server.Identifier != e.self.Name {
		return nil, fmt.Errorf("pki: name mismatch in self descriptor: '%v'", e.self.Name)
	}
	if !e.isOurLayerSane(s.cfg.Server.IsProvider) {
		return nil, fmt.Errorf("pki: self layer is invalid: %d", e.self.Layer)
	}

	// Build the maps of peers that will connect to us, and that we will
	// connect to.
	buildMap := func(layer uint8, m map[[constants.NodeIDLength]byte]*cpki.MixDescriptor) {
		var nodes []*cpki.MixDescriptor
		switch layer {
		case cpki.LayerProvider:
			nodes = e.doc.Providers
		default:
			nodes = e.doc.Topology[layer]
		}
		for _, v := range nodes {
			var id [constants.NodeIDLength]byte
			copy(id[:], v.IdentityKey.Bytes())
			m[id] = v
		}
	}
	buildMap(e.incomingLayer(), e.incoming)
	buildMap(e.outgoingLayer(), e.outgoing)

	return e, nil
}

type pki struct {
	sync.RWMutex
	sync.WaitGroup

	s    *Server
	log  *logging.Logger
	impl cpki.Client

	docs map[uint64]*pkiCacheEntry

	haltCh chan interface{}
}

func (p *pki) startWorker() {

	p.Add(1)
	go p.worker()
}

func (p *pki) halt() {
	close(p.haltCh)
	p.Wait()
}

func (p *pki) worker() {
	const (
		initialSpawnDelay = 5 * time.Second
		recheckInterval   = 1 * time.Minute
	)

	timer := time.NewTimer(initialSpawnDelay)
	defer func() {
		p.log.Debugf("Halting PKI worker.")
		timer.Stop()
		p.Done()
	}()

	if p.impl == nil {
		p.log.Warningf("No implementation is configured, disabling PKI interface.")
		return
	}
	pkiCtx, cancelFn := context.WithCancel(context.Background())
	go func() {
		select {
		case <-p.haltCh:
			cancelFn()
		case <-pkiCtx.Done():
		}
	}()
	isCanceled := func() bool {
		select {
		case <-pkiCtx.Done():
			return true
		default:
			return false
		}
	}

	// Note: The worker's start is delayed till after the Server's connector
	// is initialized, so that force updating the outgoing connection table
	// is guaranteed to work.

	for {
		select {
		case <-p.haltCh:
			p.log.Debugf("Terminating gracefully.")
			return
		case <-pkiCtx.Done():
			return
		case <-timer.C:
		}
		if !timer.Stop() {
			<-timer.C
		}

		// Fetch the PKI documents as required.
		didUpdate := false
		for _, epoch := range p.documentsToFetch() {
			d, err := p.impl.Get(pkiCtx, epoch)
			if isCanceled() {
				// Canceled mid-fetch.
				return
			}
			if err != nil {
				p.log.Warningf("Failed to fetch PKI for epoch %v: %v", epoch, err)
				continue
			}
			ent, err := newPKICacheEntry(p.s, d)
			if err != nil {
				p.log.Warningf("Failed to generate PKI cache for epoch %v: %v", epoch, err)
				continue
			}
			p.Lock()
			p.docs[epoch] = ent
			p.Unlock()
			didUpdate = true
		}
		if didUpdate {
			// Dispose of the old PKI documents.
			p.pruneDocuments()

			// If the PKI document map changed, kick the connector worker.
			p.s.connector.forceUpdate()
		}

		// XXX/pki: When it is time, generate more mix keys, and post the
		// node's descriptor to the PKI.

		timer.Reset(recheckInterval)
	}
}

func (p *pki) pruneDocuments() {
	now, _, _ := epochtime.Now()

	p.Lock()
	p.Unlock()
	for epoch := range p.docs {
		if epoch < now {
			p.log.Debugf("Discarding PKI for epoch: %v", epoch)
			delete(p.docs, epoch)
		}
		if epoch > now+1 {
			// This should NEVER happen.
			p.log.Debugf("Far future PKI document exists, clock ran backwards?: %v", epoch)
		}
	}
}

func (p *pki) documentsToFetch() []uint64 {
	const nextFetchTill = 45 * time.Minute

	ret := make([]uint64, 0, 2)
	now, _, till := epochtime.Now()

	p.RLock()
	defer p.RUnlock()

	// Fetch the document for the current epoch if it is missing.
	if _, ok := p.docs[now]; !ok {
		ret = append(ret, now)
	}

	// If it is after the time that the next PKI has been generated, fetch
	// that as well, assuming it is missing.
	if till < nextFetchTill {
		if _, ok := p.docs[now+1]; !ok {
			ret = append(ret, now+1)
		}
	}

	return ret
}

func (p *pki) docsForEpochs(epochs []uint64) []*pkiCacheEntry {
	p.RLock()
	defer p.RUnlock()

	s := make([]*pkiCacheEntry, 0, len(epochs))
	for _, epoch := range epochs {
		if e, ok := p.docs[epoch]; ok {
			s = append(s, e)
		}
	}
	return s
}

func (p *pki) docsForOutgoing() ([]*pkiCacheEntry, uint64) {
	// We sometimes but not always allow connections from nodes listed in
	// future PKI documents.
	now, elapsed, till := epochtime.Now()
	epochs := []uint64{now}
	if till < pkiEarlyConnectSlack {
		// Allow connections to new nodes 30 mins in advance of an epoch
		// transition.
		epochs = append(epochs, now+1)
	} else if elapsed < pkiLateConnectSlack {
		// Allow connections to old notes to linger for 3 mins past the epoch
		// transition.
		epochs = append(epochs, now-1)
	}

	return p.docsForEpochs(epochs), now
}

func (p *pki) authenticateIncoming(c *wire.PeerCredentials) (canSend, isValid bool) {
	const (
		earlyAcceptSlack = 30 * time.Minute
		earlySendSlack   = 2 * time.Minute
		lateSendSlack    = 2 * time.Minute
	)

	// If mix authentication is disabled, then we just allow everyone to
	// connect as a mix.
	if p.s.cfg.Debug.DisableMixAuthentication {
		p.log.Debugf("Incoming: Blindly authenticating peer: '%v'(%v).", bytesToPrintString(c.AdditionalData), ecdhToPrintString(c.PublicKey))
		return true, true
	}

	// We sometimes but not always allow connections from nodes listed in
	// future PKI documents.
	now, elapsed, till := epochtime.Now()
	epochs := []uint64{now}
	if till < pkiEarlyConnectSlack {
		// Allow connections from new nodes 30 mins in advance of an epoch
		// transition.
		epochs = append(epochs, now+1)
	} else if elapsed < pkiLateConnectSlack {
		// Allow connections from old notes to linger for 3 mins past the epoch
		// transition.
		epochs = append(epochs, now-1)
	}

	if len(c.AdditionalData) != constants.NodeIDLength {
		p.log.Debugf("Incoming: '%v' AD not an IdentityKey?.", bytesToPrintString(c.AdditionalData))
		return false, false
	}
	var id [constants.NodeIDLength]byte
	copy(id[:], c.AdditionalData)

	docs := p.docsForEpochs(epochs)
	for _, d := range docs {
		m, ok := d.incoming[id]
		if !ok {
			continue
		}
		if !bytes.Equal(m.LinkKey.Bytes(), c.PublicKey.Bytes()) {
			// The LinkKey that is being used for authentication should
			// match what is listed in the descriptor.
			p.log.Warningf("Incoming: '%v' Public Key mismatch: '%v'", bytesToPrintString(c.AdditionalData), ecdhToPrintString(c.PublicKey))
			continue
		}

		// The node is listed in a consensus that's reasonably current.
		isValid = true

		// Figure out if the node is allowed to send packets.
		switch d.doc.Epoch {
		case now:
			// The node is listed in the document for the current epoch.
			return true, true
		case now + 1:
			if till < earlySendSlack {
				// The node is listed in the document from the next epoch,
				// and it is less than slack till the transition.
				return true, true
			}
		case now - 1:
			if elapsed < lateSendSlack {
				// The node is listed in the document for the previous epoch,
				// and less than slack has past since the transition.
				return true, true
			}
		default:
		}

		// Well, this document doesn't seem to think that the node should
		// be able to send packets at us, maybe the other document if any
		// will be more forgiving.
	}

	return
}

func (p *pki) authenticateOutgoing(c *wire.PeerCredentials) (desc *cpki.MixDescriptor, canSend, isValid bool) {
	// If mix authentication is disabled, then we just blindly blast away.
	if p.s.cfg.Debug.DisableMixAuthentication {
		p.log.Debugf("Outgoing: Blindly authenticating peer: '%v'(%v).", bytesToPrintString(c.AdditionalData), ecdhToPrintString(c.PublicKey))
		return nil, true, true
	}

	// Don't need to check length here because all callers either explicitly
	// set this, or validate it (assuming it's coming from a peer).
	var id [constants.NodeIDLength]byte
	copy(id[:], c.AdditionalData)

	docs, now := p.docsForOutgoing()
	for _, d := range docs {
		m, ok := d.outgoing[id]
		if !ok {
			continue
		}
		if !bytes.Equal(m.LinkKey.Bytes(), c.PublicKey.Bytes()) {
			// The LinkKey that is being used for authentication should
			// match what is listed in the descriptor.
			p.log.Warningf("Outgoing: '%v' Public Key mismatch: '%v'", bytesToPrintString(c.AdditionalData), ecdhToPrintString(c.PublicKey))
			continue
		}

		// The node is listed in a consensus that's reasonably current.
		isValid = true
		desc = m

		// If this is the document for the current epoch, the node is listed in
		// it, and we can send packets.
		//
		// Note: This is more strict than the incoming case since the main
		// reason the slack time exists is to account for clock skew, and it's
		// all handled there.
		if d.doc.Epoch == now {
			return m, true, true
		}

		// But we're not sure if we can send packets to it yet.
	}

	return
}

func (p *pki) outgoingDestinations() map[[constants.NodeIDLength]byte]*cpki.MixDescriptor {
	docs, _ := p.docsForOutgoing()
	descMap := make(map[[constants.NodeIDLength]byte]*cpki.MixDescriptor)

	for _, d := range docs {
		for _, v := range d.outgoing {
			var id [constants.NodeIDLength]byte
			copy(id[:], v.IdentityKey.Bytes())

			// De-duplicate.
			if _, ok := descMap[id]; !ok {
				descMap[id] = v
			}
		}
	}
	return descMap
}

func (p *pki) isValidForwardDest(id *[constants.NodeIDLength]byte) bool {
	// If mix authentication is disabled, then we just queue all the packets.
	if p.s.cfg.Debug.DisableMixAuthentication {
		return true
	}

	// This doesn't need to be super accurate, just enough to prevent packets
	// destined to la-la land from being scheduled.  As a mix we should
	// basically never see packets destined for nodes not listed in the
	// current consensus unless a node gets delisted.
	p.RLock()
	defer p.RUnlock()

	now, _, _ := epochtime.Now()
	doc, ok := p.docs[now]
	if !ok {
		return false
	}
	return doc.outgoing[*id] != nil
}

func newPKI(s *Server) *pki {
	p := new(pki)
	p.s = s
	p.log = s.newLogger("pki")
	p.docs = make(map[uint64]*pkiCacheEntry)
	p.haltCh = make(chan interface{})

	// XXX/pki: Initialize the concrete implementation.

	// Note: This does not start the worker immediately since the worker can
	// make calls into the connector (on PKI updates), which is initialized
	// after the pki object.

	return p
}
