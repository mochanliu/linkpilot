// Tests for the basemap asset endpoint.
//
// The interesting property is not the bytes, it is the boundary: geometry that
// happens to be a file in the binary is served to anyone, while a device's
// position is a tenant document that is served to nobody without a session. These
// cases hold that line, and one of them holds the generator's contract -- the 50m
// tier has no projection parameters of its own, because drawing it in its own
// fitted frame would put two coastlines a few pixels apart and read as a bad map.
package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// assetReq hits the handler directly: nothing here depends on a session, so a
// cookie would only blur which endpoint is being tested.
func assetReq(t *testing.T, s *server, target, acceptEncoding string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if acceptEncoding != "" {
		r.Header.Set("Accept-Encoding", acceptEncoding)
	}
	rec := httptest.NewRecorder()
	s.handleConsoleAsset(rec, r)
	return rec
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	return out
}

func TestBasemapAssetsAreEmbedded(t *testing.T) {
	// Size floors rather than exact counts: a truncated or half-generated file is
	// the failure mode of committing a generated asset, and it still parses.
	if len(world110mJS) < 100_000 {
		t.Fatalf("110m basemap looks truncated: %d bytes", len(world110mJS))
	}
	if len(world50mJS) < 500_000 {
		t.Fatalf("50m basemap looks truncated: %d bytes", len(world50mJS))
	}
	s := string(world110mJS)
	for _, want := range []string{"window.SR_WORLD=", `"w":2000`, `"h":1040`, `"land":"M`, `"borders":"M`, `"graticule":"M`} {
		if !strings.Contains(s, want) {
			t.Errorf("110m basemap is missing %s", want)
		}
	}
	// The frame belongs to the 110m file alone. If the generator ever starts
	// fitting the two tiers separately, the 50m layer will carry these keys and
	// the map will draw its own coastline offset from the one under it.
	fifty := string(world50mJS)
	if !strings.Contains(fifty, "window.SR_WORLD_50M=") || !strings.Contains(fifty, `"land":"M`) {
		t.Fatal("50m basemap has no land path")
	}
	for _, forbidden := range []string{`"scale"`, `"tx"`, `"ty"`, `"w":`} {
		if strings.Contains(fifty, forbidden) {
			t.Errorf("the 50m tier carries %s; it must be drawn in the 110m frame", forbidden)
		}
	}
}

func TestBasemapIsServedWithoutASession(t *testing.T) {
	s := newTestServer()
	rec := assetReq(t, s, "/assets/world-110m.js", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous GET = %d, want 200 (the basemap holds no tenant data)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("content type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q; both files change with the binary", cc)
	}
	if rec.Body.String() != string(world110mJS) {
		t.Error("served body differs from the embedded bytes")
	}
	if cl, want := rec.Header().Get("Content-Length"), strconv.Itoa(len(world110mJS)); cl != want {
		t.Errorf("content length = %q, want %s", cl, want)
	}
}

func TestBasemapServesGzipOnlyWhenAsked(t *testing.T) {
	s := newTestServer()

	plain := assetReq(t, s, "/assets/world-50m.js", "identity")
	if enc := plain.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("an identity client was handed %q", enc)
	}
	if plain.Body.Len() != len(world50mJS) {
		t.Errorf("plain body is %d bytes, want %d", plain.Body.Len(), len(world50mJS))
	}

	gz := assetReq(t, s, "/assets/world-50m.js", "gzip, deflate, br")
	if enc := gz.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("browser Accept-Encoding did not get gzip, got %q", enc)
	}
	if v := gz.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Errorf("Vary = %q; a shared cache in front of the console must not mix the two", v)
	}
	if got := gunzip(t, gz.Body.Bytes()); string(got) != string(world50mJS) {
		t.Error("the gzip variant does not decompress to the embedded bytes")
	}
	// Compression is what makes the detail tier affordable: it is the reason the
	// 50m file can be fetched at all on a remote console.
	if cl, want := gz.Header().Get("Content-Length"), strconv.Itoa(len(consoleAssets["world-50m.js"].gz)); cl != want {
		t.Errorf("content length = %q, want the compressed size %s", cl, want)
	}
	if len(consoleAssets["world-50m.js"].gz)*2 > len(world50mJS) {
		t.Errorf("gzip only shrank the 50m file from %d to %d bytes", len(world50mJS),
			len(consoleAssets["world-50m.js"].gz))
	}
}

func TestBasemapRoutesAndRefusals(t *testing.T) {
	s := newTestServer()
	if rec := assetReq(t, s, "/assets/topology.json", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown asset = %d, want 404", rec.Code)
	}
	if rec := assetReq(t, s, "/assets/world-110m.js", ""); rec.Code != http.StatusOK {
		t.Errorf("known asset = %d", rec.Code)
	}
	// A name that is not in the map is a 404 by definition of the lookup, so a
	// traversal attempt cannot reach anything: there is no filesystem here.
	if rec := assetReq(t, s, "/assets/../../etc/passwd", ""); rec.Code != http.StatusNotFound {
		t.Errorf("traversal-shaped name = %d, want 404", rec.Code)
	}
	post := httptest.NewRequest(http.MethodPost, "/assets/world-110m.js", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	s.handleConsoleAsset(rec, post)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
}

// The mux is what the process actually serves, and "/" answers 404 to every path
// except the console page. This is the only case that proves the asset pattern is
// registered ahead of that catch-all.
func TestBasemapRouteIsLiveOnTheConsoleMux(t *testing.T) {
	s := newTestServer()
	h := s.adminHandler()
	r := httptest.NewRequest(http.MethodGet, "/assets/world-110m.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("through the mux = %d, want 200; \"/\" swallows everything that is not registered", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "window.SR_WORLD=") {
		t.Fatal("the mux served something that is not the basemap")
	}
	// And the catch-all still refuses to serve a page for an unknown path.
	unknown := httptest.NewRequest(http.MethodGet, "/assets/nope.js", nil)
	deny := httptest.NewRecorder()
	h.ServeHTTP(deny, unknown)
	if deny.Code != http.StatusNotFound {
		t.Errorf("unknown path through the mux = %d, want 404", deny.Code)
	}
}
