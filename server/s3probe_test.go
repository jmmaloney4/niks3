package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mic92/niks3/ratelimit"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// fakeS3 models the garden#2933 state: HEAD answers 200 with a size for
// "lost-tail" (the manifest survives), while a ranged GET answers 404. "ok"
// serves bytes, "empty" is zero-length, anything else is absent.
func fakeS3(t *testing.T) *httptest.Server {
	t.Helper()

	noKey := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>x</Message></Error>`))
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Last-Modified", "Wed, 04 Nov 2026 18:12:44 GMT")
		w.Header().Set("ETag", `"4cea9a63"`)

		switch {
		case key == "ok" && r.Method == http.MethodHead:
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(http.StatusOK)
		case key == "ok":
			w.Header().Set("Content-Range", "bytes 0-0/10")
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("x"))
		case key == "lost-tail" && r.Method == http.MethodHead:
			w.Header().Set("Content-Length", "10314086")
			w.WriteHeader(http.StatusOK)
		case key == "empty" && r.Method == http.MethodHead:
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
		case key == "empty":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>InvalidRange</Code><Message>x</Message></Error>`))
		default:
			noKey(w)
		}
	}))
}

func TestObjectExistsInS3ReadsNotHeads(t *testing.T) {
	srv := fakeS3(t)
	defer srv.Close()

	client, err := minio.New(strings.TrimPrefix(srv.URL, "http://"), &minio.Options{
		Creds: credentials.NewStaticV4("a", "b", ""), Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := &Service{MinioClient: client, Bucket: "testbucket", S3RateLimiter: ratelimit.NewAdaptiveRateLimiter(0, "s3-test")}

	for _, tc := range []struct {
		key  string
		want bool
		why  string
	}{
		{"ok", true, "readable object"},
		{"lost-tail", false, "HEAD 200 but GET 404 must count as missing (garden#2933)"},
		{"absent", false, "no such key"},
		{"empty", true, "zero-length object cannot satisfy a range; falls back to HEAD"},
	} {
		got, err := svc.objectExistsInS3(t.Context(), tc.key)
		if err != nil {
			t.Fatalf("%s: %v", tc.key, err)
		}

		if got != tc.want {
			t.Errorf("%s: got %v want %v (%s)", tc.key, got, tc.want, tc.why)
		}
	}

	// Negative control: the old HEAD-only probe calls lost-tail present, so
	// this test could have failed against v1.16.1.
	if _, err := client.StatObject(t.Context(), "testbucket", "lost-tail", minio.StatObjectOptions{}); err != nil {
		t.Fatalf("control: HEAD should succeed on lost-tail: %v", err)
	}
}
