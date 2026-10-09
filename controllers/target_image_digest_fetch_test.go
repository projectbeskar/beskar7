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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The checksum fetch behind spec.targetImageDigestURL (D-038): what it reads,
// and the limits it holds to. Whoever can create a Beskar7Machine chooses the
// URL, so every limit here is one the manager keeps for a stranger.

const (
	// digestTestImageName is the file the checksum entries below are for.
	digestTestImageName = "ubuntu-24-04-v1-36-4-test.raw"
	// digestTestImageURL is a targetImageURL whose last path segment is that name.
	digestTestImageURL = "http://images.example.invalid/os/" + digestTestImageName

	// digestTestLeakMarker is served in bad bodies, headers and entries. It must
	// never surface in a condition, a status field, an event or a log line.
	digestTestLeakMarker = "LEAK-MARKER-8c1f5d"
)

var (
	digestTestHexA = strings.Repeat("ab", 32)
	digestTestHexB = strings.Repeat("cd", 32)
	digestTestHexC = strings.Repeat("0123456789abcdef", 4)
)

// checksumServer is a TLS server standing in for a checksum host. It counts the
// requests it gets, so a spec can tell "not fetched" from "fetched and refused".
type checksumServer struct {
	*httptest.Server
	hits atomic.Int32
	// pool trusts the server's certificate, for ChecksumRootCAs.
	pool *x509.CertPool
}

func newChecksumServer(handler http.Handler) *checksumServer {
	s := &checksumServer{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		handler.ServeHTTP(w, r)
	}))
	// A client that refuses the certificate makes the server log the failed
	// handshake; that is the point of those specs, not noise to print.
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	s.pool = x509.NewCertPool()
	s.pool.AddCert(s.Certificate())
	return s
}

// servingText answers every request with body as 200 text/plain.
func servingText(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, body)
	})
}

func gnuLine(hex, name string) string { return hex + "  " + name + "\n" }

var _ = Describe("Reading a digest out of a checksum file (D-038)", func() {
	name := digestTestImageName

	DescribeTable("finds the digest of the image's file",
		func(body, want string) {
			got, err := parseChecksumFile([]byte(body), name, "sums.example.invalid")
			Expect(err).To(BeNil())
			Expect(got).To(Equal(want))
		},
		Entry("a GNU coreutils line, text mode",
			gnuLine(digestTestHexA, name), "sha256:"+digestTestHexA),
		Entry("a GNU coreutils line, binary mode",
			digestTestHexA+" *"+name+"\n", "sha256:"+digestTestHexA),
		Entry("a BSD line",
			"SHA256 ("+name+") = "+digestTestHexA+"\n", "sha256:"+digestTestHexA),
		Entry("a BSD line without the spaces openssl leaves out",
			"SHA256("+name+")= "+digestTestHexA+"\n", "sha256:"+digestTestHexA),
		Entry("a file that is one bare digest",
			digestTestHexA, "sha256:"+digestTestHexA),
		Entry("a bare digest with whitespace around it",
			"\n  \t"+digestTestHexA+" \r\n\n", "sha256:"+digestTestHexA),
		Entry("the entry named like the image among several",
			gnuLine(digestTestHexB, "other.raw")+gnuLine(digestTestHexA, name)+gnuLine(digestTestHexC, name+".sig"),
			"sha256:"+digestTestHexA),
		Entry("the entry named like the image among GNU and BSD lines",
			gnuLine(digestTestHexB, "other.raw")+"SHA256 ("+name+") = "+digestTestHexA+"\nSHA256 (third.raw) = "+digestTestHexC+"\n",
			"sha256:"+digestTestHexA),
		Entry("hex in capitals, which comes out lowercase",
			gnuLine(strings.ToUpper(digestTestHexA), name), "sha256:"+digestTestHexA),
		Entry("a name with a leading ./",
			gnuLine(digestTestHexA, "./"+name), "sha256:"+digestTestHexA),
		Entry("blank lines and comments around the entry",
			"# SHA256SUMS for v1\n\n   \n"+gnuLine(digestTestHexA, name)+"# "+gnuLine(digestTestHexB, name),
			"sha256:"+digestTestHexA),
		Entry("CRLF line endings",
			digestTestHexB+"  other.raw\r\n"+digestTestHexA+"  "+name+"\r\n", "sha256:"+digestTestHexA),
		Entry("two entries for the name that agree, whatever the case",
			gnuLine(digestTestHexA, name)+gnuLine(strings.ToUpper(digestTestHexA), name), "sha256:"+digestTestHexA),
	)

	DescribeTable("refuses what it cannot rely on",
		func(body, wantReason, leak string) {
			got, err := parseChecksumFile([]byte(body), name, "sums.example.invalid")
			Expect(err).NotTo(BeNil())
			Expect(got).To(BeEmpty())
			Expect(err.Error()).To(ContainSubstring(wantReason))
			Expect(err.Error()).To(ContainSubstring("sums.example.invalid"), "the error names the host")
			if leak != "" {
				Expect(err.Error()).NotTo(ContainSubstring(leak), "the error repeats the file")
			}
		},
		Entry("no entry for the name",
			gnuLine(digestTestHexB, "other.raw"), "no entry for", ""),
		Entry("an empty file", "", "no entry for", ""),
		Entry("only a comment", "# "+gnuLine(digestTestHexA, name), "no entry for", ""),
		Entry("an entry in a directory is not the image's: the name must equal the last path segment",
			gnuLine(digestTestHexA, "images/"+name), "no entry for", ""),
		Entry("an entry that only starts with the name",
			gnuLine(digestTestHexA, name+".sig"), "no entry for", ""),
		Entry("a BSD line for another algorithm",
			"SHA512 ("+name+") = "+digestTestHexA+"\n", "no entry for", ""),
		Entry("a bare value that is not 64 hex",
			digestTestHexA[:63], "no entry for", ""),
		Entry("two entries for the name with different digests",
			gnuLine(digestTestHexA, name)+gnuLine(digestTestHexB, name), "conflicting entries for", digestTestHexB),
		Entry("a GNU and a BSD entry that disagree",
			gnuLine(digestTestHexA, name)+"SHA256 ("+name+") = "+digestTestHexB+"\n", "conflicting entries for", digestTestHexB),
		Entry("a digest one character short",
			gnuLine(digestTestHexA[:63], name), "is not a SHA-256 digest", digestTestHexA[:63]),
		Entry("a digest that is not hex: the sample from the request carries a g",
			gnuLine("2dd1a81860938efef480d698faefgfe2eee294590f145131081ad768d7280efa", name), "is not a SHA-256 digest", "faefgfe2"),
		Entry("a SHA-512 digest",
			gnuLine(digestTestHexA+digestTestHexB, name), "is not a SHA-256 digest", digestTestHexA+digestTestHexB),
		Entry("a valid entry beside a malformed one for the same name",
			gnuLine(digestTestHexA, name)+gnuLine(digestTestLeakMarker, name), "is not a SHA-256 digest", digestTestLeakMarker),
		Entry("a BSD entry that is not hex",
			"SHA256 ("+name+") = "+digestTestLeakMarker+"\n", "is not a SHA-256 digest", digestTestLeakMarker),
	)

	DescribeTable("takes the file name from targetImageURL",
		func(targetImageURL, want string) {
			got, err := imageFileName(targetImageURL)
			Expect(err).To(BeNil())
			Expect(got).To(Equal(want))
		},
		Entry("the last path segment", "https://h.example.invalid/a/b/ubuntu.raw", "ubuntu.raw"),
		Entry("unescaped", "http://h.example.invalid/os/ubuntu%2024.raw", "ubuntu 24.raw"),
		Entry("without the query or fragment", "https://h.example.invalid/a/ubuntu.raw?token=x#frag", "ubuntu.raw"),
		Entry("with a port", "http://h.example.invalid:8080/ubuntu.raw", "ubuntu.raw"),
	)

	DescribeTable("has no name to look up in a URL without a file",
		func(targetImageURL string) {
			got, err := imageFileName(targetImageURL)
			Expect(err).NotTo(BeNil())
			Expect(got).To(BeEmpty())
		},
		Entry("no path", "https://h.example.invalid"),
		Entry("the root", "https://h.example.invalid/"),
		Entry("a directory", "https://h.example.invalid/a/b/"),
		Entry("a dot-dot", "https://h.example.invalid/a/.."),
		Entry("not a URL", "http://[::1"),
	)

	DescribeTable("accepts only an https checksum URL without credentials",
		func(raw string, wantErr string) {
			u, err := parseDigestURL(raw)
			if wantErr == "" {
				Expect(err).To(BeNil())
				Expect(u.Scheme).To(Equal("https"))
				return
			}
			Expect(err).NotTo(BeNil())
			Expect(u).To(BeNil())
			Expect(err.Error()).To(ContainSubstring(wantErr))
			Expect(err.Error()).NotTo(ContainSubstring("hunter2"), "a password in the URL is never repeated")
		},
		Entry("https", "https://sums.example.invalid/v1/SHA256SUMS", ""),
		Entry("https with a port and a query", "https://sums.example.invalid:8443/SHA256SUMS?sig=abc", ""),
		Entry("http", "http://sums.example.invalid/SHA256SUMS", "not an https URL"),
		Entry("a scheme that is not http", "file:///etc/passwd", "not an https URL"),
		Entry("no scheme", "sums.example.invalid/SHA256SUMS", "not an https URL"),
		Entry("no host", "https:///SHA256SUMS", "not an https URL"),
		Entry("credentials", "https://admin:hunter2@sums.example.invalid/SHA256SUMS", "must not contain credentials"),
		Entry("a user name only", "https://admin@sums.example.invalid/SHA256SUMS", "must not contain credentials"),
	)

	It("logs a URL without its credentials, query or fragment", func() {
		Expect(redactedURL("https://admin:hunter2@sums.example.invalid:8443/v1/SHA256SUMS?token=s3cret#frag")).
			To(Equal("https://sums.example.invalid:8443/v1/SHA256SUMS"))
		Expect(redactedURL("not a url")).To(Equal("<unparseable URL>"))
	})

	It("backs off from 30 seconds to 5 minutes", func() {
		var got []time.Duration
		for failures := 1; failures <= 8; failures++ {
			got = append(got, digestRetryDelay(failures))
		}
		Expect(got).To(Equal([]time.Duration{
			30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
			5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
		}))
	})
})

var _ = Describe("The checksum fetch's HTTP client (D-038)", func() {
	ctx := context.Background()

	fetch := func(s *checksumServer, path string) (string, *digestError) {
		return fetchTargetImageDigest(ctx, newChecksumClient(s.pool), s.URL+path, digestTestImageURL)
	}
	servers := func(handlers ...http.Handler) []*checksumServer {
		var out []*checksumServer
		for _, h := range handlers {
			s := newChecksumServer(h)
			DeferCleanup(s.Close)
			out = append(out, s)
		}
		return out
	}

	It("is its own client: verified TLS 1.2 or later, the environment proxy, ten seconds, no reuse", func() {
		c := newChecksumClient(nil)
		Expect(c).NotTo(BeIdenticalTo(http.DefaultClient))
		Expect(c.Timeout).To(Equal(10 * time.Second))
		Expect(c.CheckRedirect).NotTo(BeNil())
		tr, ok := c.Transport.(*http.Transport)
		Expect(ok).To(BeTrue())
		Expect(tr).NotTo(BeIdenticalTo(http.DefaultTransport))
		Expect(tr.TLSClientConfig.InsecureSkipVerify).To(BeFalse())
		Expect(tr.TLSClientConfig.MinVersion).To(Equal(uint16(tls.VersionTLS12)))
		Expect(tr.TLSClientConfig.RootCAs).To(BeNil(), "no pool means the system roots")
		Expect(tr.Proxy).NotTo(BeNil(), "an environment proxy is fine for a public, credential-free fetch")
		Expect(tr.DisableKeepAlives).To(BeTrue())
		Expect(tr.DisableCompression).To(BeTrue(), "a decoded body would make the size cap one on the compressed bytes")
	})

	It("reads the entry of the image from a verified HTTPS server", func() {
		s := servers(servingText(gnuLine(digestTestHexB, "other.raw") + gnuLine(digestTestHexA, digestTestImageName)))[0]
		got, err := fetch(s, "/SHA256SUMS")
		Expect(err).To(BeNil())
		Expect(got).To(Equal("sha256:" + digestTestHexA))
		Expect(s.hits.Load()).To(Equal(int32(1)))
	})

	It("sends no credentials and asks for no compression", func() {
		var got http.Header
		s := servers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			_, _ = io.WriteString(w, digestTestHexA)
		}))[0]
		_, err := fetch(s, "/SHA256SUMS")
		Expect(err).To(BeNil())
		Expect(got.Get("Authorization")).To(BeEmpty())
		Expect(got.Get("Cookie")).To(BeEmpty())
		Expect(got.Get("Accept-Encoding")).To(BeEmpty())
		Expect(got.Get("User-Agent")).To(Equal("beskar7-controller"))
	})

	It("refuses a certificate it does not trust, with the system roots", func() {
		s := servers(servingText(digestTestHexA))[0]
		got, err := fetchTargetImageDigest(ctx, newChecksumClient(nil), s.URL+"/SHA256SUMS", digestTestImageURL)
		Expect(err).NotTo(BeNil())
		Expect(got).To(BeEmpty())
		Expect(err.reason).To(Equal("TLS certificate not trusted"))
		Expect(err.host).To(Equal(strings.TrimPrefix(s.URL, "https://")))
	})

	It("refuses a certificate made for another host", func() {
		s := servers(servingText(digestTestHexA))[0]
		// The test certificate is for 127.0.0.1, ::1 and example.com.
		url := strings.Replace(s.URL, "127.0.0.1", "localhost", 1) + "/SHA256SUMS"
		_, err := fetchTargetImageDigest(ctx, newChecksumClient(s.pool), url, digestTestImageURL)
		Expect(err).NotTo(BeNil())
		Expect(err.reason).To(Equal("TLS certificate is for a different host"))
	})

	It("refuses a server that only speaks TLS older than 1.2", func() {
		old := httptest.NewUnstartedServer(servingText(digestTestHexA))
		old.Config.ErrorLog = log.New(io.Discard, "", 0)
		old.TLS = &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}
		old.StartTLS()
		DeferCleanup(old.Close)
		pool := x509.NewCertPool()
		pool.AddCert(old.Certificate())
		_, err := fetchTargetImageDigest(ctx, newChecksumClient(pool), old.URL+"/SHA256SUMS", digestTestImageURL)
		Expect(err).NotTo(BeNil(), "a TLS 1.1 handshake must fail")
	})

	It("refuses a server that is not speaking TLS", func() {
		plain := httptest.NewServer(servingText(digestTestHexA))
		DeferCleanup(plain.Close)
		// The scheme is https, the server answers in clear text.
		_, err := fetchTargetImageDigest(ctx, newChecksumClient(nil), "https://"+strings.TrimPrefix(plain.URL, "http://")+"/SHA256SUMS", digestTestImageURL)
		Expect(err).NotTo(BeNil())
		Expect(err.reason).To(Equal("not an HTTPS server"))
	})

	It("reports an HTTP error by its code alone, and repeats nothing the server said", func() {
		s := servers(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Detail", digestTestLeakMarker)
			http.Error(w, digestTestLeakMarker, http.StatusNotFound)
		}))[0]
		_, err := fetch(s, "/SHA256SUMS")
		Expect(err).NotTo(BeNil())
		Expect(err.reason).To(Equal("HTTP 404"))
		Expect(err.Error()).NotTo(ContainSubstring(digestTestLeakMarker))
	})

	It("takes only a 200 as an answer", func() {
		s := servers(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))[0]
		_, err := fetch(s, "/SHA256SUMS")
		Expect(err).NotTo(BeNil())
		Expect(err.reason).To(Equal("HTTP 204"))
	})

	Describe("redirects", func() {
		It("follows a redirect to the same host", func() {
			s := servers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/old" {
					http.Redirect(w, r, "/v1/SHA256SUMS", http.StatusFound)
					return
				}
				_, _ = io.WriteString(w, gnuLine(digestTestHexA, digestTestImageName))
			}))[0]
			got, err := fetch(s, "/old")
			Expect(err).To(BeNil())
			Expect(got).To(Equal("sha256:" + digestTestHexA))
			Expect(s.hits.Load()).To(Equal(int32(2)))
		})

		It("follows an absolute redirect to the same host and port", func() {
			s := servers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/old" {
					http.Redirect(w, r, "https://"+r.Host+"/v1/SHA256SUMS", http.StatusMovedPermanently)
					return
				}
				_, _ = io.WriteString(w, digestTestHexA)
			}))[0]
			_, err := fetch(s, "/old")
			Expect(err).To(BeNil())
		})

		It("follows three redirects and refuses a fourth", func() {
			chain := func(hops int) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var n int
					if _, err := fmt.Sscanf(r.URL.Path, "/hop/%d", &n); err == nil && n < hops {
						http.Redirect(w, r, fmt.Sprintf("/hop/%d", n+1), http.StatusFound)
						return
					}
					_, _ = io.WriteString(w, digestTestHexA)
				})
			}
			s := servers(chain(3), chain(4))
			_, err := fetch(s[0], "/hop/0")
			Expect(err).To(BeNil(), "three redirects are within the limit")
			Expect(s[0].hits.Load()).To(Equal(int32(4)))

			_, err = fetch(s[1], "/hop/0")
			Expect(err).NotTo(BeNil())
			Expect(err.reason).To(Equal("more than 3 redirects"))
			Expect(s[1].hits.Load()).To(Equal(int32(4)), "the fourth redirect is not followed")
		})

		It("refuses a redirect to another host and never connects to it", func() {
			target := servers(servingText(gnuLine(digestTestHexB, digestTestImageName)))[0]
			s := servers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Same address, another port: another origin.
				http.Redirect(w, r, target.URL+"/SHA256SUMS", http.StatusFound)
			}))[0]
			got, err := fetch(s, "/SHA256SUMS")
			Expect(err).NotTo(BeNil())
			Expect(got).To(BeEmpty())
			Expect(err.reason).To(Equal("redirect to another host refused"))
			Expect(target.hits.Load()).To(BeZero(), "the redirect target must not be contacted")
		})

		It("refuses a redirect to another host name", func() {
			s := servers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://elsewhere.example.invalid/SHA256SUMS", http.StatusFound)
			}))[0]
			_, err := fetch(s, "/SHA256SUMS")
			Expect(err).NotTo(BeNil())
			Expect(err.reason).To(Equal("redirect to another host refused"))
		})

		It("refuses a redirect to plain HTTP on the same host", func() {
			plain := httptest.NewServer(servingText(digestTestHexA))
			DeferCleanup(plain.Close)
			s := servers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, plain.URL+"/SHA256SUMS", http.StatusFound)
			}))[0]
			_, err := fetch(s, "/SHA256SUMS")
			Expect(err).NotTo(BeNil())
			Expect(err.reason).To(Equal("redirect to another host refused"))
		})
	})

	Describe("the size cap", func() {
		limit := digestFetchMaxBytes

		// padded is a valid checksum file of exactly n bytes: the entry, then comment lines.
		padded := func(n int) string {
			body := gnuLine(digestTestHexA, digestTestImageName)
			for len(body) < n {
				body += "#" + strings.Repeat("x", 78) + "\n"
			}
			return body[:n]
		}

		It("accepts a file of exactly 64 KiB", func() {
			s := servers(servingText(padded(limit)))[0]
			got, err := fetch(s, "/SHA256SUMS")
			Expect(err).To(BeNil())
			Expect(got).To(Equal("sha256:" + digestTestHexA))
		})

		It("refuses a file one byte over, sent in chunks", func() {
			s := servers(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				body := padded(limit + 1)
				// Flushing first leaves out Content-Length, so the cap is the reader's.
				_, _ = io.WriteString(w, body[:100])
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, body[100:])
			}))[0]
			got, err := fetch(s, "/SHA256SUMS")
			Expect(err).NotTo(BeNil())
			Expect(got).To(BeEmpty())
			Expect(err.reason).To(ContainSubstring("checksum file too large"))
		})

		It("refuses a file that declares itself too large without reading it", func() {
			s := servers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(limit+1))
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))[0]
			started := time.Now()
			_, err := fetch(s, "/SHA256SUMS")
			Expect(err).NotTo(BeNil())
			Expect(err.reason).To(ContainSubstring("checksum file too large"))
			Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second), "refused on the header, with no body sent")
		})

		It("never buffers an endless body", func() {
			s := servers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				chunk := []byte(strings.Repeat("z", 4096))
				for r.Context().Err() == nil {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
			}))[0]
			started := time.Now()
			_, err := fetch(s, "/SHA256SUMS")
			Expect(err).NotTo(BeNil())
			Expect(err.reason).To(ContainSubstring("checksum file too large"))
			Expect(time.Since(started)).To(BeNumerically("<", 5*time.Second), "stopped at the cap, not at the timeout")
		})
	})

	It("gives up on a server that never answers, after ten seconds", func() {
		release := make(chan struct{})
		s := servers(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))[0]
		DeferCleanup(func() { close(release) })
		started := time.Now()
		_, err := fetch(s, "/SHA256SUMS")
		Expect(err).NotTo(BeNil())
		Expect(err.reason).To(Equal("timed out after 10s"))
		Expect(time.Since(started)).To(BeNumerically(">=", 9*time.Second))
		Expect(time.Since(started)).To(BeNumerically("<", 15*time.Second))
	})

	It("gives up on a server that stalls in the middle of the body", func() {
		release := make(chan struct{})
		s := servers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "# slow\n")
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))[0]
		DeferCleanup(func() { close(release) })
		_, err := fetch(s, "/SHA256SUMS")
		Expect(err).NotTo(BeNil())
		Expect(err.reason).To(Equal("timed out after 10s"))
	})

	It("reports a refused connection without the address it tried", func() {
		s := newChecksumServer(servingText(digestTestHexA))
		url := s.URL + "/SHA256SUMS?token=" + digestTestLeakMarker
		s.Close()
		_, err := fetchTargetImageDigest(ctx, newChecksumClient(s.pool), url, digestTestImageURL)
		Expect(err).NotTo(BeNil())
		Expect(err.reason).To(Equal("cannot connect"))
		Expect(err.Error()).NotTo(ContainSubstring(digestTestLeakMarker), "the query stays out of the error")
	})
})
