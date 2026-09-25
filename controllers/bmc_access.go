/*
Copyright 2024 The Beskar7 Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/publicsuffix"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/projectbeskar/beskar7/api/v1beta2"
)

// bmcAccess is what a Redfish client for a host may be built with: the
// credentials, and how the connection to the BMC is secured.
type bmcAccess struct {
	username string
	password string
	caBundle []byte
	insecure bool
}

// String and GoString keep the credentials out of anything that formats a
// bmcAccess, such as a log line or a wrapped error.
func (a bmcAccess) String() string   { return "bmcAccess{credentials redacted}" }
func (a bmcAccess) GoString() string { return a.String() }

// bmcAccessError is a refusal from resolveBMCAccess. reason is the
// RedfishConnectionReady reason it maps to. The message is written to the
// host's status, so it names the Secret, the annotation and the address's
// host, and never the credentials or the raw address (which could carry
// userinfo).
type bmcAccessError struct {
	reason string
	msg    string
}

func (e *bmcAccessError) Error() string { return e.msg }

func refuseBMCAccess(reason, format string, args ...any) error {
	return &bmcAccessError{reason: reason, msg: fmt.Sprintf(format, args...)}
}

// bmcAccessReason returns the RedfishConnectionReady reason for an error from
// resolveBMCAccess.
func bmcAccessReason(err error) string {
	var refusal *bmcAccessError
	if errors.As(err, &refusal) {
		return refusal.reason
	}
	return infrav1.RedfishConnectionFailedReason
}

// resolveBMCAccess decides whether the credentials host's credentialsSecretRef
// names may be sent to its redfishConnection.address, and returns them only if
// so (D-030, SEC-16). Both readers of BMC credentials call it before they build
// a Redfish client: PhysicalHostReconciler.reconcileNormal and
// Beskar7MachineReconciler.getRedfishClientForHost. gofish sends the
// credentials as Basic auth on the first authenticated request, so anything
// checked after the client exists is too late.
//
// Anyone allowed to patch or create a PhysicalHost chooses its address, so the
// address cannot vouch for itself: the Secret has to. It must list the
// address's host in BMCAddressesAnnotation, and opt in with
// BMCInsecureTransportAnnotation before its credentials travel over http:// or
// to a BMC whose certificate is not verified, since either lets whoever is on
// the path read them. Every refusal fails closed: no Secret, no annotation, a
// malformed list, an address the list does not name, or an address that is
// not a plain http(s) URL all return a bmcAccessError and no credentials.
func resolveBMCAccess(ctx context.Context, c client.Reader, host *infrav1.PhysicalHost) (bmcAccess, error) {
	conn := host.Spec.RedfishConnection
	secretName := conn.CredentialsSecretRef
	if secretName == "" {
		return bmcAccess{}, refuseBMCAccess(infrav1.MissingCredentialsReason,
			"failed to retrieve credentials: redfishConnection.credentialsSecretRef is empty")
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: host.Namespace, Name: secretName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return bmcAccess{}, refuseBMCAccess(infrav1.MissingCredentialsReason,
				"failed to retrieve credentials: Secret %q not found", secretName)
		}
		return bmcAccess{}, refuseBMCAccess(infrav1.MissingCredentialsReason,
			"failed to retrieve credentials: failed to get Secret %q: %v", secretName, err)
	}

	bmcHost, plaintext, err := parseBMCAddress(conn.Address)
	if err != nil {
		return bmcAccess{}, refuseBMCAccess(infrav1.CredentialsNotAuthorizedReason,
			"BMC credentials not sent: redfishConnection.address %v", err)
	}

	listed, annotated := secret.Annotations[BMCAddressesAnnotation]
	if !annotated {
		return bmcAccess{}, refuseBMCAccess(infrav1.CredentialsNotAuthorizedReason,
			"BMC credentials not sent: Secret %q has no %s annotation; if %q is this host's BMC, "+
				"add the annotation to the Secret listing it (an IP address, CIDR, hostname or *.suffix)",
			secretName, BMCAddressesAnnotation, bmcHost)
	}
	allowList, err := parseBMCAllowList(listed)
	if err != nil {
		return bmcAccess{}, refuseBMCAccess(infrav1.CredentialsNotAuthorizedReason,
			"BMC credentials not sent: the %s annotation on Secret %q %v; it authorises no address until it is fixed",
			BMCAddressesAnnotation, secretName, err)
	}
	if !allowList.allows(bmcHost) {
		return bmcAccess{}, refuseBMCAccess(infrav1.CredentialsNotAuthorizedReason,
			"BMC credentials not sent: the %s annotation on Secret %q does not list %q; "+
				"if it is this host's BMC, add it to the annotation",
			BMCAddressesAnnotation, secretName, bmcHost)
	}

	insecure := ptr.Deref(conn.InsecureSkipVerify, false)
	if (plaintext || insecure) && secret.Annotations[BMCInsecureTransportAnnotation] != "true" {
		transport := "insecureSkipVerify: true"
		if plaintext {
			transport = "an http:// address"
		}
		return bmcAccess{}, refuseBMCAccess(infrav1.CredentialsNotAuthorizedReason,
			"BMC credentials not sent: redfishConnection uses %s, which does not verify the BMC; "+
				"set %s: \"true\" on Secret %q to allow it",
			transport, BMCInsecureTransportAnnotation, secretName)
	}

	if err := validateRedfishTLSCombination(insecure, conn.CABundleSecretRef); err != nil {
		return bmcAccess{}, refuseBMCAccess(infrav1.InsecureCABundleConflictReason, "%s", err.Error())
	}

	username, hasUsername := secret.Data["username"]
	password, hasPassword := secret.Data["password"]
	switch {
	case !hasUsername:
		return bmcAccess{}, refuseBMCAccess(infrav1.MissingCredentialsReason,
			"failed to retrieve credentials: username not found in Secret %q", secretName)
	case !hasPassword:
		return bmcAccess{}, refuseBMCAccess(infrav1.MissingCredentialsReason,
			"failed to retrieve credentials: password not found in Secret %q", secretName)
	}

	caBundle, err := fetchRedfishCABundle(ctx, c, host)
	if err != nil {
		return bmcAccess{}, refuseBMCAccess(infrav1.CABundleFetchFailedReason, "%s", err.Error())
	}

	return bmcAccess{
		username: string(username),
		password: string(password),
		caBundle: caBundle,
		insecure: insecure,
	}, nil
}

// parseBMCAddress returns the lower-cased host of a redfishConnection.address
// and whether it is plain http. It accepts only an absolute http or https URL
// with a host and no userinfo, and a host that is an IP address without a zone
// or a valid DNS name. The CRD pattern already narrows the field; this is what
// the credentials depend on, so it does not rely on the schema. The Redfish
// client parses the address with the same url.Parse, so the host checked here
// is the host it connects to. Errors never repeat the address: it could carry
// a password in its userinfo.
func parseBMCAddress(address string) (string, bool, error) {
	u, err := url.Parse(address)
	if err != nil {
		return "", false, errors.New("is not a valid URL")
	}
	var plaintext bool
	switch u.Scheme {
	case "https":
	case "http":
		plaintext = true
	default:
		return "", false, errors.New("must be an http:// or https:// URL")
	}
	if u.User != nil {
		return "", false, errors.New("must not carry userinfo (user:password@)")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false, errors.New("has no host")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return "", false, errors.New("must not be an IPv6 address with a zone")
		}
		return host, plaintext, nil
	}
	if len(validation.IsDNS1123Subdomain(host)) > 0 {
		return "", false, errors.New("has a host that is neither an IP address nor a valid DNS name")
	}
	return host, plaintext, nil
}

// bmcAllowList is a parsed BMCAddressesAnnotation. An IP address host is
// matched only against addrs and prefixes, and a DNS name only against names
// and suffixes, so no CIDR can match a name that happens to start with an IP
// address and no wildcard can match an IP address whose text ends in its
// suffix.
type bmcAllowList struct {
	addrs    []netip.Addr
	prefixes []netip.Prefix
	names    []string
	// suffixes hold "*.b.c" as ".b.c".
	suffixes []string
}

// parseBMCAllowList parses a BMCAddressesAnnotation value. Entries are
// separated by commas and/or whitespace. The whole list is rejected if any
// entry is malformed, or if there are none: a typo must never widen or silently
// narrow what the Secret authorises, and the operator sees the error instead.
func parseBMCAllowList(value string) (bmcAllowList, error) {
	entries := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(entries) == 0 {
		return bmcAllowList{}, errors.New("lists no address")
	}
	var list bmcAllowList
	for i, entry := range entries {
		if err := list.add(entry); err != nil {
			// The entry is quoted by position, not content: this reaches the
			// status of any host that names the Secret, and whoever can patch a
			// host cannot necessarily read the Secret.
			return bmcAllowList{}, fmt.Errorf("is malformed: entry %d %v", i+1, err)
		}
	}
	return list, nil
}

func (l *bmcAllowList) add(entry string) error {
	if strings.Contains(entry, "/") {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return errors.New("is not a valid CIDR")
		}
		// allows compares the unmapped address, so a mapped prefix could
		// never match: accepting it would silently narrow the list.
		if prefix.Addr().Is4In6() {
			return errors.New("is an IPv4-mapped IPv6 CIDR; list the IPv4 CIDR instead")
		}
		// A range this wide reaches addresses anyone who can run a pod or
		// hold an address on the network may answer from.
		if limit := minBMCPrefixBits(prefix.Addr()); prefix.Bits() < limit {
			return fmt.Errorf("is wider than /%d", limit)
		}
		l.prefixes = append(l.prefixes, prefix.Masked())
		return nil
	}
	if addr, err := netip.ParseAddr(entry); err == nil {
		if addr.Zone() != "" {
			return errors.New("is an IPv6 address with a zone, which is not supported")
		}
		l.addrs = append(l.addrs, addr.Unmap())
		return nil
	}
	name := strings.ToLower(entry)
	if suffix, ok := strings.CutPrefix(name, "*."); ok {
		if len(validation.IsDNS1123Subdomain(suffix)) > 0 {
			return errors.New("is not a valid *.suffix wildcard")
		}
		// Anyone can register a name under a public suffix, and with no
		// caBundleSecretRef the BMC's certificate is checked against the
		// system roots, so *.com would verify a certificate for a name the
		// attacker bought. Unknown TLDs (lab, svc, local) count as public
		// suffixes too, which is what keeps *.svc from matching every
		// Service in the cluster.
		if ps, _ := publicsuffix.PublicSuffix(suffix); ps == suffix {
			return errors.New("is a wildcard over a public suffix")
		}
		l.suffixes = append(l.suffixes, "."+suffix)
		return nil
	}
	if len(validation.IsDNS1123Subdomain(name)) > 0 {
		return errors.New("is not an IP address, CIDR, hostname or *.suffix wildcard")
	}
	l.names = append(l.names, name)
	return nil
}

// minBMCPrefixBits is the shortest CIDR prefix an allow-list entry may have.
func minBMCPrefixBits(addr netip.Addr) int {
	if addr.Is4() {
		return 8
	}
	return 32
}

// allows reports whether host, as parseBMCAddress returns it, is on the list.
// An IPv4-mapped IPv6 address is compared as the IPv4 address it reaches.
func (l bmcAllowList) allows(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return false
		}
		addr = addr.Unmap()
		for _, a := range l.addrs {
			if a == addr {
				return true
			}
		}
		for _, p := range l.prefixes {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}
	host = strings.ToLower(host)
	for _, name := range l.names {
		if name == host {
			return true
		}
	}
	for _, suffix := range l.suffixes {
		if len(host) > len(suffix) && strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}
