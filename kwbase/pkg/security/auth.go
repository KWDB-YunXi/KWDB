// Copyright 2015 The Cockroach Authors.
// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This software (KWDB) is licensed under Mulan PSL v2.
// You can use this software according to the terms and conditions of the Mulan PSL v2.
// You may obtain a copy of Mulan PSL v2 at:
//          http://license.coscl.org.cn/MulanPSL2
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the Mulan PSL v2 for more details.

package security

import (
	"crypto/tls"
	"crypto/x509"
	"strings"

	"gitee.com/kwbasedb/kwbase/pkg/util/syncutil"
	"github.com/pkg/errors"
)

const (
	// NodeUser is used by nodes for intra-cluster traffic.
	NodeUser = "node"
	// RootUser is the default cluster administrator.
	RootUser = "root"
)

var certPrincipalMap struct {
	syncutil.RWMutex
	m map[string]string
}

// UserAuthHook authenticates a user based on their username and whether their
// connection originates from a client or another node in the cluster. It
// returns an optional func that is run at connection close.
type UserAuthHook func(string, bool) (connClose func(), _ error)

// SetCertPrincipalMap sets the global principal map. Each entry in the mapping
// list must either be empty or have the format <source>:<dest>. The principal
// map is used to transform principal names found in the Subject.CommonName or
// DNS-type SubjectAlternateNames fields of certificates.
func SetCertPrincipalMap(mappings []string) error {
	m := make(map[string]string, len(mappings))
	for _, v := range mappings {
		if v == "" {
			continue
		}
		parts := strings.Split(v, ":")
		if len(parts) != 2 {
			return errors.Errorf("invalid <cert-principal>:<db-principal> mapping: %q", v)
		}
		m[parts[0]] = parts[1]
	}
	certPrincipalMap.Lock()
	certPrincipalMap.m = m
	certPrincipalMap.Unlock()
	return nil
}

func transformPrincipal(commonName string) string {
	certPrincipalMap.RLock()
	mappedName, ok := certPrincipalMap.m[commonName]
	certPrincipalMap.RUnlock()
	if !ok {
		return commonName
	}
	return mappedName
}

func getCertificatePrincipals(cert *x509.Certificate) []string {
	results := make([]string, 0, 1+len(cert.DNSNames))
	results = append(results, transformPrincipal(cert.Subject.CommonName))
	for _, name := range cert.DNSNames {
		results = append(results, transformPrincipal(name))
	}
	return results
}

// GetCertificateUsers extract the users from a client certificate.
func GetCertificateUsers(tlsState *tls.ConnectionState) ([]string, error) {
	if tlsState == nil {
		return nil, errors.Errorf("request is not using TLS")
	}
	if len(tlsState.PeerCertificates) == 0 {
		return nil, errors.Errorf("no client certificates in request")
	}
	// The go server handshake code verifies the first certificate, using
	// any following certificates as intermediates. See:
	// https://github.com/golang/go/blob/go1.8.1/src/crypto/tls/handshake_server.go#L723:L742
	peerCert := tlsState.PeerCertificates[0]
	return getCertificatePrincipals(peerCert), nil
}

// ContainsUser returns true if the specified user is present in the list of
// users.
func ContainsUser(user string, users []string) bool {
	for i := range users {
		if user == users[i] {
			return true
		}
	}
	return false
}

// UserAuthCertHook builds an authentication hook based on the security
// mode and client certificate.
func UserAuthCertHook(insecureMode bool, tlsState *tls.ConnectionState) (UserAuthHook, error) {
	var certUsers []string

	if !insecureMode {
		var err error
		certUsers, err = GetCertificateUsers(tlsState)
		if err != nil {
			return nil, err
		}
	}

	return func(requestedUser string, clientConnection bool) (func(), error) {
		// TODO(marc): we may eventually need stricter user syntax rules.
		if len(requestedUser) == 0 {
			return nil, errors.New("user is missing")
		}

		if !clientConnection && requestedUser != NodeUser {
			return nil, errors.Errorf("user %s is not allowed", requestedUser)
		}

		// If running in insecure mode, we have nothing to verify it against.
		if insecureMode {
			return nil, nil
		}

		// The client certificate user must match the requested user,
		// except if the certificate user is NodeUser, which is allowed to
		// act on behalf of all other users.
		if !ContainsUser(requestedUser, certUsers) && !ContainsUser(NodeUser, certUsers) {
			return nil, errors.Errorf("requested user is %s, but certificate is for %s", requestedUser, certUsers)
		}

		return nil, nil
	}, nil
}

// UserAuthPasswordHook builds an authentication hook based on the security
// mode, password, and its potentially matching hash.
func UserAuthPasswordHook(insecureMode bool, password string, hashedPassword []byte) UserAuthHook {
	return func(requestedUser string, clientConnection bool) (func(), error) {
		if len(requestedUser) == 0 {
			return nil, errors.New("user is missing")
		}

		if !clientConnection {
			return nil, errors.New("password authentication is only available for client connections")
		}

		if insecureMode {
			return nil, nil
		}

		// If the requested user has an empty password, disallow authentication.
		if len(password) == 0 || CompareHashAndPassword(hashedPassword, password) != nil {
			return nil, errors.Errorf(ErrPasswordUserAuthFailed, requestedUser)
		}

		return nil, nil
	}
}

// ErrPasswordUserAuthFailed is the error template for failed password auth
// of a user. It should be used when the password is incorrect or the user
// does not exist.
const ErrPasswordUserAuthFailed = "password authentication failed for user %s"
