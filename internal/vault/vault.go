// Copyright 2019 - MinIO, Inc. All rights reserved.
// Use of this source code is governed by the AGPLv3
// license that can be found in the LICENSE file.

// Package vault implements a secret key store that
// stores secret keys as key-value entries on the
// Hashicorp Vault K/V secret backend.
//
// Vault is a KMS implementation with many featues.
// This packages only leverages the key-value store.
// For an introduction to Vault see: https://www.vaultproject.io/
// For an K/V API overview see: https://www.vaultproject.io/api/secret/kv/kv-v1.html
package vault

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/minio/kes"
)

// AppRole holds the Vault AppRole
// authentication credentials and
// a duration after which the
// authentication should be retried
// whenever it fails.
type AppRole struct {
	ID     string // The AppRole  ID
	Secret string // The Approle secret ID
	Retry  time.Duration
}

// Store is a key-value store that saves key-value
// pairs as entries on Vault's K/V secret backend.
type Store struct {
	// Addr is the HTTP address of the Vault server.
	Addr string

	// Location is the location on Vault's K/V store
	// where this KeyStore will save secret keys.
	//
	// It can be used to assign an unique or shared
	// prefix. For instance one or more KeyStore can
	// store secret keys under /keys/my-app/. In this
	// case you may set KeyStore.Location = "key/my-app".
	Location string

	// AppRole contains the Vault AppRole authentication
	// credentials.
	AppRole AppRole

	// StatusPingAfter is the duration after which
	// the KeyStore will check the status of the Vault
	// server. Particularly, this status information
	// is used to determine whether the Vault server
	// has been sealed resp. unsealed again.
	StatusPingAfter time.Duration

	// ErrorLog specifies an optional logger for errors
	// when files cannot be opened, deleted or contain
	// invalid content.
	// If nil, logging is done via the log package's
	// standard logger.
	ErrorLog *log.Logger

	// Path to the mTLS client private key to authenticate to
	// the Vault server.
	ClientKeyPath string

	// Path to the mTLS client certificate to authenticate to
	// the Vault server.
	ClientCertPath string

	// Path to the root CA certificate(s) used to verify the
	// TLS certificate of the Vault server. If empty, the
	// host's root CA set is used.
	CAPath string

	// The Vault namespace used to separate and isolate different
	// organizations / tenants at the same Vault instance. If
	// non-empty, the Vault client will send the
	//   X-Vault-Namespace: Namespace
	// HTTP header on each request. For more information see:
	// https://www.vaultproject.io/docs/enterprise/namespaces/index.html
	Namespace string

	client *vaultapi.Client
	sealed bool
}

// Authenticate tries to establish a connection to
// a Vault server using the approle credentials.
// It returns an error if no connection could be
// established - for instance because of invalid
// authentication credentials.
func (s *Store) Authenticate(context context.Context) error {
	tlsConfig := &vaultapi.TLSConfig{
		ClientKey:  s.ClientKeyPath,
		ClientCert: s.ClientCertPath,
	}
	if s.CAPath != "" {
		stat, err := os.Stat(s.CAPath)
		if err != nil {
			return fmt.Errorf("Failed to open '%s': %v", s.CAPath, err)
		}
		if stat.IsDir() {
			tlsConfig.CAPath = s.CAPath
		} else {
			tlsConfig.CACert = s.CAPath
		}
	}

	config := vaultapi.DefaultConfig()
	config.Address = s.Addr
	config.ConfigureTLS(tlsConfig)
	client, err := vaultapi.NewClient(config)
	if err != nil {
		return err
	}
	if s.Namespace != "" {
		// We must only set the namespace if it is not
		// empty. If namespace == "" the vault client
		// will send an empty namespace HTTP header -
		// which is not what we want.
		client.SetNamespace(s.Namespace)
	}

	s.client = client

	status, err := s.client.Sys().Health()
	if err != nil {
		return err
	}
	s.sealed = status.Sealed

	var token string
	var ttl time.Duration
	if !status.Sealed {
		token, ttl, err = s.authenticate(s.AppRole)
		if err != nil {
			return err
		}
		s.client.SetToken(token)
	}

	go s.checkStatus(context, s.StatusPingAfter)
	go s.renewAuthToken(context, s.AppRole, ttl)
	return nil
}

var errSealed = kes.NewError(http.StatusForbidden, "key store is sealed")

// Get returns the value associated with the given key.
// If no entry for the key exists it returns kes.ErrKeyNotFound.
func (s *Store) Get(key string) (string, error) {
	if s.client == nil {
		s.log(errNoConnection)
		return "", errNoConnection
	}
	if s.sealed {
		return "", errSealed
	}

	location := fmt.Sprintf("/kv/%s/%s", s.Location, key)
	entry, err := s.client.Logical().Read(location)
	if err != nil || entry == nil {
		// Vault will not return an error if e.g. the key existed but has
		// been deleted. However, it will return (nil, nil) in this case.
		if err == nil && entry == nil {
			return "", kes.ErrKeyNotFound
		}
		s.logf("vault: failed to read '%s': %v", location, err)
		return "", err
	}

	// Verify that we got a well-formed response from Vault
	v, ok := entry.Data[key]
	if !ok || v == nil {
		s.logf("vault: failed to read '%s': entry exists but no secret key is present", location)
		return "", errors.New("vault: K/V entry does not contain any value")
	}
	value, ok := v.(string)
	if !ok {
		s.logf("vault: failed to read '%s': invalid K/V format", location)
		return "", errors.New("vault: invalid K/V entry format")
	}
	return value, nil
}

// Create creates the given key-value pair at Vault if and only
// if the given key does not exist. If such an entry already exists
// it returns kes.ErrKeyExists.
func (s *Store) Create(key, value string) error {
	if s.client == nil {
		s.log(errNoConnection)
		return errNoConnection
	}
	if s.sealed {
		return errSealed
	}

	// We try to check whether key exists on the K/V store.
	// If so, we must not overwrite it.
	location := fmt.Sprintf("/kv/%s/%s", s.Location, key)

	// Vault will return nil for the secret as well as a nil-error
	// if the specified entry does not exist.
	// More specifically the Vault server + client behaves as following:
	//  - If the entry does not exist (b/c it never existed) the server
	//    returns 404 and the client returns the tuple (nil, nil).
	//  - If the entry does not exist (b/c it existed before but has
	//    been deleted) the server returns 404 but response with a
	//    "secret". The client will still parse the response body (even
	//    though 404) and return (nil, nil) if the body is empty or
	//    the secret contains no data (and no "warnings" or "errors")
	//
	// Therefore, we check whether the client returns a nil error
	// and a non-nil "secret". In this case, the secret key already
	// exists.
	// But when the client returns an error it does not mean that
	// the entry does not exist but that some other error (e.g.
	// network error) occurred.
	switch secret, err := s.client.Logical().Read(location); {
	case err == nil && secret != nil:
		return kes.ErrKeyExists
	case err != nil:
		s.logf("vault: failed to create '%s': %v", location, err)
		return err
	}

	// Finally, we create the value since it seems that it
	// doesn't exist. However, this is just an assumption since
	// another key server may have created that key in the meantime.
	// Since there is now way we can detect that reliable we require
	// that whoever has the permission to create keys does that in
	// a non-racy way.
	_, err := s.client.Logical().Write(location, map[string]interface{}{
		key: value,
	})
	if err != nil {
		s.logf("vault: failed to create '%s': %v", location, err)
		return err
	}
	return nil
}

// Delete removes a the value associated with the given key
// from Vault, if it exists.
func (s *Store) Delete(key string) error {
	if s.client == nil {
		s.log(errNoConnection)
		return errNoConnection
	}
	if s.sealed {
		return errSealed
	}

	// Vault will not return an error if an entry does not
	// exist. Instead, it responds with 204 No Content and
	// no body. In this case the client also returns a nil-error
	// Therefore, we can just try to delete it in any case.
	location := fmt.Sprintf("/kv/%s/%s", s.Location, key)
	_, err := s.client.Logical().Delete(location)
	if err != nil {
		s.logf("vault: failed to delete '%s': %v", location, err)
	}
	return err
}

func (s *Store) authenticate(login AppRole) (token string, ttl time.Duration, err error) {
	secret, err := s.client.Logical().Write("auth/approle/login", map[string]interface{}{
		"role_id":   login.ID,
		"secret_id": login.Secret,
	})
	if err != nil || secret == nil {
		if err == nil {
			// TODO: return non-nil error
		}
		return token, ttl, err
	}

	token, err = secret.TokenID()
	if err != nil {
		return token, ttl, err
	}

	ttl, err = secret.TokenTTL()
	if err != nil {
		return token, ttl, err
	}
	return token, ttl, err
}

func (s *Store) checkStatus(ctx context.Context, delay time.Duration) {
	if delay == 0 {
		delay = 10 * time.Second
	}
	var timer *time.Timer
	for {
		status, err := s.client.Sys().Health()
		if err == nil {
			s.sealed = status.Sealed
		}

		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Store) renewAuthToken(ctx context.Context, login AppRole, ttl time.Duration) {
	if login.Retry == 0 {
		login.Retry = 5 * time.Second
	}
	for {
		// If Vault is sealed we have to wait
		// until it is unsealed again.
		// The Vault status is checked by another go routine
		// constantly by querying the Vault health status.
		for s.sealed {
			timer := time.NewTimer(1 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		// If the TTL is 0 we cannot renew the token.
		// Therefore, we try to re-authenticate and
		// get a new token. We repeat that until we
		// successfully authenticate and got a token.
		if ttl == 0 {
			var (
				token string
				err   error
			)
			token, ttl, err = s.authenticate(login)
			if err != nil {
				ttl = 0
				timer := time.NewTimer(login.Retry)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			s.client.SetToken(token) // SetToken is safe to call from different go routines
		}

		// Now the client has token with a non-zero TTL
		// such tht we can renew it. We repeat that until
		// the renewable process fails once. In this case
		// we try to re-authenticate again.
		timer := time.NewTimer(ttl / 2)
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			secret, err := s.client.Auth().Token().RenewSelf(int(ttl.Seconds()))
			if err != nil || secret == nil {
				break
			}
			if ok, err := secret.TokenIsRenewable(); !ok || err != nil {
				break
			}
			ttl, err := secret.TokenTTL()
			if err != nil || ttl == 0 {
				break
			}
			timer.Reset(ttl / 2)
		}
		ttl = 0
	}
}

// errNoConnection is the error returned and logged by
// the key store if the vault client hasn't been initialized.
//
// This error is returned by Create, Get, Delete, a.s.o.
// in case of an invalid configuration - i.e. when Authenticate()
// hasn't been called.
var errNoConnection = errors.New("vault: no connection to vault server")

func (s *Store) log(v ...interface{}) {
	if s.ErrorLog == nil {
		log.Println(v...)
	} else {
		s.ErrorLog.Println(v...)
	}
}

func (s *Store) logf(format string, v ...interface{}) {
	if s.ErrorLog == nil {
		log.Printf(format, v...)
	} else {
		s.ErrorLog.Printf(format, v...)
	}
}