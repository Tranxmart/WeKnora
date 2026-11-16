package txmdocuments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/Tencent/WeKnora/internal/txmdocuments/gen/document_service_api"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func httpResponse(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{
		StatusCode:    status,
		Header:        headers,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func protoResponse(t *testing.T, message proto.Message) *http.Response {
	t.Helper()
	body, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return httpResponse(200, string(body), http.Header{"Content-Type": {"application/proto"}})
}

func testConfig() Config {
	return Config{
		IdentityServiceURL: "http://identity.service:8080",
		IdentityTokenFile:  "/projected/token",
		IdentityAudience:   "https://identity.txm.internal",
		ServiceURL:         "http://documents.service:8080",
		Resource:           defaultResource,
	}
}

func goodIdentity(_ *http.Request) (*http.Response, error) {
	return httpResponse(200, `{"access_token":"exchanged-secret","token_type":"Bearer","expires_in":100}`, nil), nil
}

func testEntry(content string) *Entry {
	digest := sha256.Sum256([]byte(content))
	return &Entry{
		Document: &Document{
			DocumentId:               proto.String("doc-id"),
			CurrentDocumentVersionId: proto.String("version-id"),
		},
		CurrentVersion: &Version{
			DocumentId:        proto.String("doc-id"),
			DocumentVersionId: proto.String("version-id"),
			BlobDigest:        proto.String("sha256:" + hex.EncodeToString(digest[:])),
			SizeBytes:         proto.Int64(int64(len(content))),
		},
	}
}

func newTestClient(t *testing.T, deps dependencies) *Client {
	t.Helper()
	if deps.identityTransport == nil {
		deps.identityTransport = roundTripFunc(goodIdentity)
	}
	if deps.readFile == nil {
		deps.readFile = func(string) ([]byte, error) { return []byte("projected-secret"), nil }
	}
	c, err := newClient(testConfig(), deps)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigIsLazyAndDefaults(t *testing.T) {
	for _, name := range []string{
		"IDENTITY_SERVICE_URL", "IDENTITY_TOKEN_FILE", "IDENTITY_AUDIENCE",
		"DOCUMENT_SERVICE_URL", "DOCUMENT_SERVICE_RESOURCE",
	} {
		t.Setenv(name, "")
	}
	cfg := ConfigFromEnv()
	if cfg.ServiceURL != defaultServiceURL || cfg.Resource != defaultResource {
		t.Fatal("missing endpoint defaults")
	}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "IDENTITY_SERVICE_URL") {
		t.Fatalf("configuration error: %v", err)
	}
	var used atomic.Bool
	_, err := newClient(
		testConfig(),
		dependencies{
			readFile: func(string) ([]byte, error) { used.Store(true); return nil, errors.New("unexpected read") },
			identityTransport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				used.Store(true)
				return nil, errors.New("unexpected network")
			}),
		},
	)
	if err != nil || used.Load() {
		t.Fatalf("New must not read tokens or access network: %v", err)
	}
	for _, raw := range []string{
		"http://identity/?secret=hidden", "http://user:password@identity",
		"file:///identity", "http://identity/#hidden",
	} {
		cfg = testConfig()
		cfg.IdentityServiceURL = raw
		if _, err := New(cfg); err == nil || strings.Contains(err.Error(), "hidden") ||
			strings.Contains(err.Error(), "password") {
			t.Fatalf("unsafe endpoint validation: %v", err)
		}
	}
}

func TestListAndBatchUseCanonicalBindingsAndExchangedCredential(t *testing.T) {
	var identities, rpcs int
	c := newTestClient(t, dependencies{
		identityTransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			identities++
			if r.URL.String() != "http://identity.service:8080/token" || r.Method != "POST" ||
				r.Header.Get("Authorization") != "" {
				t.Fatal("invalid identity endpoint or credential")
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{
				"grant_type":         "urn:ietf:params:oauth:grant-type:token-exchange",
				"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
				"subject_token":      "projected-secret", "resource": defaultResource,
			} {
				if r.PostForm.Get(key) != want {
					t.Errorf("form %s mismatch", key)
				}
			}
			if r.PostForm.Get("audience") != "" {
				t.Error("identity resource exchange does not use OAuth audience")
			}
			return goodIdentity(r)
		}),
		rpcTransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rpcs++
			if r.Header.Get("Authorization") != "Bearer exchanged-secret" {
				t.Fatal("RPC did not use exchanged credential")
			}
			body, _ := io.ReadAll(r.Body)
			if strings.HasSuffix(r.URL.Path, "/ListDocuments") {
				var req api.ListDocumentsRequest
				if err := proto.Unmarshal(body, &req); err != nil {
					t.Fatal(err)
				}
				if req.GetPageSize() != 100 || req.GetPageToken() != "opaque" || len(req.GetSourceTypes()) != 1 ||
					req.GetSourceTypes()[0] != SourceManual {
					t.Fatalf("incorrect page request: %v", &req)
				}
				return protoResponse(
					t,
					&api.ListDocumentsResponse{
						Documents:     []*Entry{testEntry("x")},
						NextPageToken: proto.String("next"),
					},
				), nil
			}
			var req api.BatchGetDocumentsRequest
			if err := proto.Unmarshal(body, &req); err != nil {
				t.Fatal(err)
			}
			if len(req.GetDocumentIds()) != 1 || req.GetDocumentIds()[0] != "doc-id" {
				t.Fatal("batch request mismatch")
			}
			return protoResponse(
				t,
				&api.BatchGetDocumentsResponse{
					Documents: []*Summary{{DocumentId: proto.String("doc-id"), Access: AccessReadable.Enum()}},
				},
			), nil
		}),
	})
	page, err := c.ListDocuments(context.Background(), "opaque", []SourceType{SourceManual})
	if err != nil || len(page.Documents) != 1 || page.NextPageToken != "next" {
		t.Fatalf("list: %v %v", page, err)
	}
	summaries, err := c.BatchGetDocuments(context.Background(), []string{"doc-id"})
	if err != nil || len(summaries) != 1 || summaries[0].GetAccess() != AccessReadable {
		t.Fatalf("batch: %v %v", summaries, err)
	}
	if identities != 1 || rpcs != 2 {
		t.Fatalf("cache calls identity=%d rpc=%d", identities, rpcs)
	}
}

func TestTokenCacheRotationRefreshFallbackAndExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	file := "first-jwt"
	var subjects []string
	fail := false
	c := newTestClient(
		t,
		dependencies{
			now:      func() time.Time { return now },
			readFile: func(string) ([]byte, error) { return []byte(file), nil },
			identityTransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				_ = r.ParseForm()
				subjects = append(subjects, r.PostForm.Get("subject_token"))
				if fail {
					return httpResponse(500, "sensitive-server-body", nil), nil
				}
				return goodIdentity(r)
			}),
		},
	)
	ctx := context.Background()
	token, err := c.tokens.get(ctx)
	if err != nil || token != "exchanged-secret" {
		t.Fatal(err)
	}
	now = now.Add(49 * time.Second)
	_, _ = c.tokens.get(ctx)
	if len(subjects) != 1 {
		t.Fatal("refreshed before half-life")
	}
	file = "rotated-jwt"
	now = now.Add(time.Second)
	_, err = c.tokens.get(ctx)
	if err != nil || len(subjects) != 2 || subjects[1] != "rotated-jwt" {
		t.Fatalf("rotation: %v %v", subjects, err)
	}
	fail = true
	now = now.Add(50 * time.Second)
	token, err = c.tokens.get(ctx)
	if err != nil || token != "exchanged-secret" {
		t.Fatal("unexpired credential must survive failed refresh")
	}
	now = now.Add(50 * time.Second)
	token, err = c.tokens.get(ctx)
	if err == nil || token != "" || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("expired credential reused or error leaked: %q %v", token, err)
	}
}

func TestTokenCacheConcurrentExchangeSerialized(t *testing.T) {
	var count atomic.Int32
	c := newTestClient(
		t,
		dependencies{
			identityTransport: roundTripFunc(
				func(r *http.Request) (*http.Response, error) { count.Add(1); return goodIdentity(r) },
			),
		},
	)
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			if token, err := c.tokens.get(context.Background()); err != nil || token != "exchanged-secret" {
				t.Errorf("get: %v", err)
			}
		})
	}
	group.Wait()
	if count.Load() != 1 {
		t.Fatalf("exchanges=%d", count.Load())
	}
}

func TestUnauthenticatedClearsCacheAndRetriesOnlyOnce(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "recover", false: "bounded"}[success], func(t *testing.T) {
			var identityCalls, rpcCalls int
			c := newTestClient(
				t,
				dependencies{
					identityTransport: roundTripFunc(
						func(r *http.Request) (*http.Response, error) { identityCalls++; return goodIdentity(r) },
					),
					rpcTransport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
						rpcCalls++
						if success && rpcCalls == 2 {
							return protoResponse(t, &api.ListDocumentsResponse{}), nil
						}
						return httpResponse(
							401,
							`{"code":"unauthenticated","message":"signed-url?secret=hidden"}`,
							http.Header{"Content-Type": {"application/json"}},
						), nil
					}),
				},
			)
			_, err := c.ListDocuments(context.Background(), "", nil)
			if success && err != nil || !success && err == nil || identityCalls != 2 || rpcCalls != 2 {
				t.Fatalf("retry calls %d %d: %v", identityCalls, rpcCalls, err)
			}
			if err != nil &&
				(strings.Contains(err.Error(), "hidden") || connect.CodeOf(err) != connect.CodeUnauthenticated) {
				t.Fatalf("leaked or lost error code: %v", err)
			}
		})
	}
}

func TestOriginGuardAndAllRedirectsBlocked(t *testing.T) {
	u, _ := url.Parse("http://identity.internal/token")
	var requests int
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		requests++
		return httpResponse(307, "", http.Header{"Location": {"http://evil.invalid/steal?secret=hidden"}}), nil
	})
	client := originClient(u, transport, nil)
	req, _ := http.NewRequest("POST", u.String(), strings.NewReader("secret"))
	req.Header.Set("Authorization", "Bearer secret")
	response, err := client.Do(req)
	if err != nil || response.StatusCode != 307 || requests != 1 {
		t.Fatalf("redirect followed: requests=%d %v", requests, err)
	}
	for _, target := range []string{
		"http://evil.invalid/token", "https://identity.internal/token",
		"http://identity.internal/other", "http://identity.internal/token?secret=x",
	} {
		req, _ = http.NewRequest("POST", target, nil)
		if _, err := client.Do(req); err == nil {
			t.Fatalf("origin/path escape %s", target)
		}
	}
	c := newTestClient(t, dependencies{rpcTransport: transport})
	_, err = c.rpc.Resolve(
		context.Background(),
		connect.NewRequest(&api.ResolveRequest{Url: proto.String("https://example.com")}),
	)
	if err == nil || requests != 1 {
		t.Fatalf("unauthorized RPC was sent: %d %v", requests, err)
	}
}

func downloadClient(
	t *testing.T,
	entry *Entry,
	download roundTripFunc,
	mutate func(int, *api.CreateDocumentDownloadResponse),
) (*Client, *int) {
	t.Helper()
	calls := 0
	jar, _ := cookiejar.New(nil)
	origin, _ := url.Parse("https://blob.example")
	jar.SetCookies(origin, []*http.Cookie{{Name: "secret-cookie", Value: "credential"}})
	c := newTestClient(
		t,
		dependencies{
			downloadClient: &http.Client{Transport: download, Jar: jar},
			rpcTransport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				body, _ := io.ReadAll(r.Body)
				var req api.CreateDocumentDownloadRequest
				if err := proto.Unmarshal(body, &req); err != nil {
					t.Fatal(err)
				}
				if req.GetDocumentId() != entry.GetDocument().GetDocumentId() ||
					req.GetDocumentVersionId() != entry.GetCurrentVersion().GetDocumentVersionId() {
					t.Fatal("download selected another version")
				}
				capability := &api.CreateDocumentDownloadResponse{
					DocumentVersion: proto.Clone(entry.GetCurrentVersion()).(*Version),
					DownloadUrl: proto.String(
						"https://blob.example/file?signature=secret&response-content-type=application%2Foctet-stream",
					),
					ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
				}
				if mutate != nil {
					mutate(calls, capability)
				}
				return protoResponse(t, capability), nil
			}),
		},
	)
	return c, &calls
}

func TestDownloadExactSignedURLWithoutCredentials(t *testing.T) {
	entry := testEntry("markdown")
	c, calls := downloadClient(t, entry, func(r *http.Request) (*http.Response, error) {
		expectedURL := "https://blob.example/file?signature=secret&" +
			"response-content-type=application%2Foctet-stream"
		if r.URL.String() != expectedURL {
			t.Fatal("signed URL was changed")
		}
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" ||
			r.Header.Get("Accept-Encoding") != "identity" {
			t.Fatal("download included credentials or unsafe encoding")
		}
		return httpResponse(200, "markdown", nil), nil
	}, nil)
	body, err := c.Download(context.Background(), entry, 100)
	if err != nil || string(body) != "markdown" || *calls != 1 {
		t.Fatalf("download: %q %v calls=%d", body, err, *calls)
	}
}

func TestDownloadRejectsCorruptPartialEncodedAndRedirectResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		headers    http.Header
		length     int64
	}{
		{"digest", "modified", 200, nil, 8},
		{"truncated", "mark", 200, nil, -1},
		{"oversized", "markdown-extra", 200, nil, -1},
		{"declared-length", "markdown", 200, nil, 9},
		{"partial", "markdown", 206, nil, 8},
		{"range", "markdown", 200, http.Header{"Content-Range": {"bytes 0-7/10"}}, 8},
		{"compressed", "markdown", 200, http.Header{"Content-Encoding": {"gzip"}}, 8},
		{"redirect", "", 302, http.Header{"Location": {"https://evil.invalid/?secret=hidden"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := testEntry("markdown")
			downloads := 0
			c, _ := downloadClient(t, entry, func(*http.Request) (*http.Response, error) {
				downloads++
				r := httpResponse(tc.status, tc.body, tc.headers)
				r.ContentLength = tc.length
				return r, nil
			}, nil)
			body, err := c.Download(context.Background(), entry, 100)
			if err == nil || body != nil || downloads != 1 || strings.Contains(err.Error(), "hidden") ||
				strings.Contains(err.Error(), "signature") {
				t.Fatalf("invalid download accepted or leaked: %q %v count=%d", body, err, downloads)
			}
		})
	}
}

func TestDownloadRenewsOnceWithSameVersion(t *testing.T) {
	for _, mode := range []string{"expired", "forbidden", "always-forbidden"} {
		t.Run(mode, func(t *testing.T) {
			entry := testEntry("markdown")
			downloads := 0
			c, calls := downloadClient(t, entry, func(*http.Request) (*http.Response, error) {
				downloads++
				if mode == "always-forbidden" || mode == "forbidden" && downloads == 1 {
					return httpResponse(403, "secret-expired-response", nil), nil
				}
				return httpResponse(200, "markdown", nil), nil
			}, func(call int, capability *api.CreateDocumentDownloadResponse) {
				if mode == "expired" && call == 1 {
					capability.ExpiresAt = timestamppb.New(time.Now().Add(-time.Second))
				}
			})
			body, err := c.Download(context.Background(), entry, 100)
			if *calls != 2 || mode != "always-forbidden" && (err != nil || string(body) != "markdown") ||
				mode == "always-forbidden" && (err == nil || strings.Contains(err.Error(), "secret")) {
				t.Fatalf("renew count=%d body=%q err=%v", *calls, body, err)
			}
		})
	}
}

func TestDownloadMetadataMismatchAndSizeLimitBeforeDownload(t *testing.T) {
	for _, field := range []string{"document", "version", "digest", "size", "missing-size"} {
		t.Run(field, func(t *testing.T) {
			entry := testEntry("markdown")
			downloads := 0
			c, _ := downloadClient(
				t,
				entry,
				func(*http.Request) (*http.Response, error) { downloads++; return nil, errors.New("unexpected") },
				func(_ int, capability *api.CreateDocumentDownloadResponse) {
					switch field {
					case "document":
						capability.DocumentVersion.DocumentId = proto.String("other")
					case "version":
						capability.DocumentVersion.DocumentVersionId = proto.String("other")
					case "digest":
						capability.DocumentVersion.BlobDigest = proto.String(strings.Repeat("a", 71))
					case "size":
						capability.DocumentVersion.SizeBytes = proto.Int64(100)
					case "missing-size":
						capability.DocumentVersion.SizeBytes = nil
					}
				},
			)
			if _, err := c.Download(context.Background(), entry, 100); err == nil || downloads != 0 {
				t.Fatalf("metadata mismatch accepted: %v downloads=%d", err, downloads)
			}
		})
	}
	entry := testEntry("markdown")
	c, calls := downloadClient(
		t,
		entry,
		func(*http.Request) (*http.Response, error) { t.Fatal("unexpected download"); return nil, nil },
		nil,
	)
	if _, err := c.Download(context.Background(), entry, 7); err == nil || *calls != 0 {
		t.Fatalf("oversized listing not rejected before RPC: %v", err)
	}
}

func TestDownloadTransportErrorsAreSanitized(t *testing.T) {
	entry := testEntry("markdown")
	c, _ := downloadClient(t, entry, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("https://blob.example/file?signature=hidden")
	}, nil)
	_, err := c.Download(context.Background(), entry, 100)
	if err == nil || strings.Contains(err.Error(), "hidden") || strings.Contains(err.Error(), "blob.example") {
		t.Fatalf("transport error leaked: %v", err)
	}
}

func TestIdentityFailuresNeverSendProjectedCredentialToRPC(t *testing.T) {
	for _, response := range []string{
		`{"access_token":"secret","token_type":"Bearer","expires_in":0}`,
		`{"access_token":"secret","token_type":"Bearer","expires_in":-1}`,
		`{"access_token":"secret","token_type":"Bearer","expires_in":1.5}`,
		`{"access_token":"secret","token_type":"Bearer","expires_in":9223372036854775807}`,
		`{"access_token":"secret","token_type":"Basic","expires_in":100}`,
		`{"access_token":"secret\r\ninjected","token_type":"Bearer","expires_in":100}`,
		`{"access_token":"secret","token_type":"Bearer","expires_in":100,"issued_token_type":"untrusted"}`,
		`not json with raw-secret`,
	} {
		c := newTestClient(
			t,
			dependencies{
				identityTransport: roundTripFunc(
					func(*http.Request) (*http.Response, error) { return httpResponse(200, response, nil), nil },
				),
				rpcTransport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Fatal("RPC was sent despite identity failure")
					return nil, nil
				}),
			},
		)
		_, err := c.ListDocuments(context.Background(), "", nil)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("failed exchange accepted or leaked: %v", err)
		}
	}
}

func TestIdentityRedirectAndReadFailuresAreSanitized(t *testing.T) {
	for _, mode := range []string{"redirect", "transport", "file"} {
		t.Run(mode, func(t *testing.T) {
			var identityCalls int
			deps := dependencies{identityTransport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				identityCalls++
				if mode == "transport" {
					return nil, errors.New("https://identity/token?credential=hidden")
				}
				return httpResponse(
					302,
					"body-secret",
					http.Header{"Location": {"http://evil.invalid/token?secret=hidden"}},
				), nil
			}), rpcTransport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("unexpected RPC")
				return nil, nil
			})}
			if mode == "file" {
				deps.readFile = func(string) ([]byte, error) { return nil, errors.New("path-and-secret-hidden") }
			}
			c := newTestClient(t, deps)
			_, err := c.ListDocuments(context.Background(), "", nil)
			if err == nil || strings.Contains(err.Error(), "hidden") || mode != "file" && identityCalls != 1 ||
				mode == "file" && identityCalls != 0 {
				t.Fatalf("identity failure or redirect unsafe: %v calls=%d", err, identityCalls)
			}
		})
	}
}

func TestDownloadEmptyVersion(t *testing.T) {
	entry := testEntry("")
	c, _ := downloadClient(
		t,
		entry,
		func(*http.Request) (*http.Response, error) { return httpResponse(200, "", nil), nil },
		nil,
	)
	body, err := c.Download(context.Background(), entry, 0)
	if err != nil || len(body) != 0 {
		t.Fatalf("empty version download: %q %v", body, err)
	}
}
