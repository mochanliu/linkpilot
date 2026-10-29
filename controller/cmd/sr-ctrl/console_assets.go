package main

// The world basemap, served as a static asset.
//
// Two facts make this the one console URL that needs no session, and they are
// the reason the geometry is a separate file rather than a field of
// /api/underlay/topology: it holds no tenant data at all (it is public-domain
// Natural Earth, the same bytes for every customer and for an empty tenant), and
// it cannot change while the process runs, because it is compiled in. What a
// device's position says about that device lives only in the topology document,
// and that one is still gated.
//
// The bytes come from tools/world-basemap/gen.mjs and are committed, so building
// sr-ctrl needs neither node nor a network -- the air-gap delivery constraint
// again.

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"net/http"
	"strconv"
	"strings"
)

//go:embed assets/world-110m.js
var world110mJS []byte

//go:embed assets/world-50m.js
var world50mJS []byte

type worldAsset struct {
	raw []byte
	gz  []byte
}

// worldAssets is built once, at init: compressing 680 KB at startup is far
// cheaper than doing it per request, and these files are served for the whole
// life of the process.
var worldAssets = map[string]worldAsset{
	"world-110m.js": newWorldAsset(world110mJS),
	"world-50m.js":  newWorldAsset(world50mJS),
}

func newWorldAsset(raw []byte) worldAsset {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	// A bytes.Buffer never fails a Write, and Close only flushes.
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return worldAsset{raw: raw, gz: buf.Bytes()}
}

// handleConsoleAsset serves /assets/<name>. Only the two basemaps exist, so the
// lookup against a fixed map is also the whole path check: nothing here touches
// the filesystem.
func (s *server) handleConsoleAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	asset, ok := worldAssets[strings.TrimPrefix(r.URL.Path, "/assets/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, encoded := asset.raw, ""
	// A yes/no check on the token rather than a full Accept-Encoding parse: a
	// client that says "gzip;q=0" gets gzip anyway, and every browser that can
	// load this page can decode it.
	if strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") {
		body, encoded = asset.gz, "gzip"
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Vary", "Accept-Encoding")
	// no-store, like the console page itself: both change when the binary does,
	// and a 50m coastline cached from the previous build would draw out of
	// register with a 110m frame it was never fitted to.
	w.Header().Set("Cache-Control", "no-store")
	if encoded != "" {
		w.Header().Set("Content-Encoding", encoded)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}
