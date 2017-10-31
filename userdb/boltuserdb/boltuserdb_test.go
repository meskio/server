// boltuserdb_test.go - boltuserdb tests.
// Copyright (C) 2017  Yawning Angel
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

package boltuserdb

import (
	"crypto/rand"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/katzenpost/core/crypto/ecdh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDB = "userdb.db"

var (
	tmpDir     string
	testDBPath string

	testUsernames = []string{"alice", "bob"}
	testUsers     map[string]*ecdh.PublicKey
)

func TestBoltUserDB(t *testing.T) {
	t.Logf("Temp Dir: %v", tmpDir)
	if ok := t.Run("create", doTestCreate); ok {

	} else {
		t.Errorf("create tests failed, skipping load test")
	}

	os.RemoveAll(tmpDir)
}

func doTestCreate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	d, err := New(testDBPath)
	require.NoError(err, "New()")
	defer d.Close()

	for u, k := range testUsers {
		err = d.Add([]byte(u), k)
		require.NoErrorf(err, "Add(%v)", u)
	}

	for u, k := range testUsers {
		assert.True(d.IsValid([]byte(u), k), "IsValid('%s', k)", u)
	}
	assert.False(d.IsValid([]byte("mallory"), testUsers["alice"]), "IsValid('mallory', k)")
}

func doTestLoad(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	d, err := New(testDBPath)
	require.NoError(err, "New() load")
	defer d.Close()

	for u, k := range testUsers {
		assert.True(d.IsValid([]byte(u), k), "IsValid('%s', k)", u)
	}
	assert.False(d.IsValid([]byte("mallory"), testUsers["alice"]), "IsValid('mallory', k)")
}

func init() {
	var err error
	tmpDir, err = ioutil.TempDir("", "boltuserdb_tests")
	if err != nil {
		panic(err)
	}
	testDBPath = filepath.Join(tmpDir, testDB)

	testUsers = make(map[string]*ecdh.PublicKey)
	for _, v := range testUsernames {
		privKey, err := ecdh.NewKeypair(rand.Reader)
		if err != nil {
			panic(err)
		}
		testUsers[v] = privKey.PublicKey()
	}
}