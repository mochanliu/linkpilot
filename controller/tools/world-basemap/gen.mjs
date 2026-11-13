#!/usr/bin/env node
// gen.mjs -- build the world basemap that the console embeds.
//
// WHAT IT PRODUCES (committed, so building sr-ctrl never needs node):
//   controller/cmd/sr-ctrl/assets/world-110m.js   ~132 KB  land + borders + graticule
//   controller/cmd/sr-ctrl/assets/world-50m.js    ~680 KB  land only, fetched on demand
//
// Both files are one assignment to a window global holding SVG path strings.
// There is no library at runtime: the console's only projection code is the
// 12-line Natural Earth I formula that places devices, and the assertion at the
// end of this script proves that formula reproduces d3's output to well under a
// pixel, which is what lets the two layers and the device points share one frame.
//
// RUN IT
//   node gen.mjs [--vendor DIR] [--out DIR]
//
// node is only needed to REGENERATE, i.e. to change resolution, latitude crop,
// viewport size or precision. For this machine, which has no node on PATH:
//   ELECTRON_RUN_AS_NODE=1 "/Applications/Qoder CN.app/Contents/MacOS/Qoder CN" gen.mjs
//
// --vendor DIR must hold the six input files listed in README.md. It defaults
// to ./vendor next to this script.

import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import zlib from 'node:zlib';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const args = process.argv.slice(2);
const opt = (name, dflt) => {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && args[i + 1] ? args[i + 1] : dflt;
};
const VENDOR = path.resolve(opt('vendor', path.join(HERE, 'vendor')));
const OUT = path.resolve(opt('out', path.join(HERE, '..', '..', 'cmd', 'sr-ctrl', 'assets')));

// The reference frame. Both tiers are projected with one fitted projection built
// from the 110m layer, so a 50m coastline drawn later lands exactly on top of the
// 110m one instead of drifting by however much the two bounding boxes differ.
const W = 2000;
const H = 1040;
const PAD = 4;
const LAT_MIN_DEG = -60; // Antarctica's shelf edge; below this is blank ocean.

const need = (rel) => {
  const p = path.join(VENDOR, rel);
  if (!fs.existsSync(p)) {
    console.error(`missing ${p}\nPut the input files listed in README.md into ${VENDOR} (or pass --vendor).`);
    process.exit(1);
  }
  return p;
};
const readJSON = (rel) => JSON.parse(fs.readFileSync(need(rel), 'utf8'));

// d3-geo and topojson-client ship UMD builds that attach to globalThis, so they
// are evaluated in a sandbox rather than imported. Nothing from the map data is
// ever executed, and this script never writes to the vendor directory.
const ctx = {
  console, Math, JSON, Number, Object, Array, String, Symbol, Error, TypeError,
  RegExp, Float64Array, Int32Array, Uint8Array, Buffer, isNaN, isFinite, parseInt, parseFloat,
};
ctx.globalThis = ctx;
vm.createContext(ctx);
for (const f of ['d3-array.min.js', 'd3-geo.min.js', 'topojson-client.min.js']) {
  vm.runInContext(fs.readFileSync(need(f), 'utf8'), ctx, { filename: f });
}
const d3 = ctx.d3;
const topojson = ctx.topojson;
if (!d3 || !d3.geoNaturalEarth1 || !topojson || !topojson.merge) {
  console.error('the vendor builds did not define d3 / topojson; check README.md for the exact files');
  process.exit(1);
}

const countries = readJSON('countries-110m.json');
const land110 = readJSON('land-110m.json');
const land50 = readJSON('land-50m.json');

const landGeo110 = topojson.feature(land110, land110.objects.land).features[0].geometry;
const landGeo50 = topojson.feature(land50, land50.objects.land).features[0].geometry;
// Country outlines, merged into one geometry: separate per-country paths would
// draw every border twice (each neighbour strokes the same shared arc) and the
// stroke would then be visibly darker along shared edges.
const borderGeo = topojson.merge(countries, countries.objects.countries.geometries);

// PRE-CLIP: d3's default for this projection, geoClipAntimeridian. It SPLITS a
// ring where it crosses the dateline, which is what makes Chukotka and Fiji end
// at the seam instead of being joined by a straight line.
//
// This used to be geoClipRectangle(-180, LAT_MIN, 180, 90), chosen to crop the
// latitude. That rectangle covers the whole 360 degrees of longitude, so both
// sides of the seam are "inside" and nothing is ever split: every ring crossing
// 180 got one long horizontal segment across the entire map -- a stripe through
// Russia at 65-71N and through Fiji at 16S, while the rest of the coastline was
// fine. Cropping is now done by the FIT below, not by the clipper.
const projection = d3.geoNaturalEarth1();

// FIT: the crop rectangle's outline, as three polylines (bottom edge and the two
// meridians). Only its bounds are used. The meridians stop a thousandth of a
// degree short of 180 so no vertex sits exactly on the seam, and they run
// through the equator, where the projection is widest -- fitting the outline is
// therefore the same as fitting "the whole world down to LAT_MIN_DEG".
// Land below LAT_MIN_DEG (the Antarctic shelf edge) is simply projected below
// the frame and clipped by the viewport, which is exactly the crop we want.
const E = 179.999;
const frameOutlines = {
  type: 'FeatureCollection',
  features: [
    [[-E, LAT_MIN_DEG], [E, LAT_MIN_DEG]],
    [[E, LAT_MIN_DEG], [E, 90]],
    [[-E, LAT_MIN_DEG], [-E, 90]],
  ].map((coordinates) => ({ type: 'Feature', properties: {}, geometry: { type: 'LineString', coordinates } })),
};
projection.fitExtent([[PAD, PAD], [W - PAD, H - PAD]], frameOutlines);
const geoPath = d3.geoPath(projection);
const feature = (geometry) => ({ type: 'Feature', properties: {}, geometry });

// One decimal: at 12x zoom a 0.1px error in the source is 1.2px on screen, and
// the coastlines are already coarser than that at this resolution.
const round = (s) => s.replace(/-?\d+\.\d+/g, (x) => (+x).toFixed(1));
const render = (geometry) => round(geoPath(feature(geometry)));

const layers110 = {
  land: render(landGeo110),
  borders: render(borderGeo),
  graticule: render(d3.geoGraticule().step([20, 20])()),
};
const layers50 = { land: render(landGeo50) };

// A ring that wraps the map instead of being split at the seam shows up as one
// segment thousands of pixels wide at constant y -- a horizontal stripe through
// the whole map. That is precisely the bug this file used to ship (a stripe
// through Russia at 65-71N), and it is invisible to the projection check above,
// so it gets its own check: no segment inside one subpath may run wider than a
// quarter of the frame without changing latitude.
const widestRun = (d) => {
  let worst = 0, where = null;
  for (const sub of d.split('M').slice(1)) {
    const pts = [...sub.matchAll(/(-?\d+(?:\.\d+)?),(-?\d+(?:\.\d+)?)/g)].map((m) => [+m[1], +m[2]]);
    for (let i = 1; i < pts.length; i++) {
      const [x0, y0] = pts[i - 1], [x1, y1] = pts[i];
      // Only what the viewport can show counts. Antarctica's closing edge at the
      // south pole is such a run, and it lands below y=H by construction.
      if (y0 < 0 || y0 > H) continue;
      if (Math.abs(y1 - y0) < 1 && Math.abs(x1 - x0) > worst) {
        worst = Math.abs(x1 - x0);
        where = [x0, y0, x1, y1];
      }
    }
  }
  return { worst, where };
};
let widest = 0;
for (const [name, layers] of [['110m', layers110], ['50m', layers50]]) {
  for (const [layer, d] of Object.entries(layers)) {
    const { worst, where } = widestRun(d);
    widest = Math.max(widest, worst);
    if (worst > W / 4) {
      console.error(`  ${name} ${layer}: a ring runs ${worst.toFixed(0)} px without changing latitude ` +
        `(${where.map((v) => v.toFixed(0)).join(', ')}); the dateline is not being split`);
    }
  }
}
console.log(`  widest horizontal run inside one subpath = ${widest.toFixed(0)} px (frame ${W})`);
if (widest > W / 4) {
  console.error('the basemap still has a wrapped ring; a stripe will show up across the map');
  process.exit(1);
}

// ---------------------------------------------------------------- the check
// The console places devices with this hand-rolled forward form of the same
// projection. If it ever disagreed with d3, every marker would sit a few pixels
// off its own country -- a mistake that looks like a bad coordinate, not like a
// bug. This assertion is why the file is generated rather than hand-drawn.
function naturalEarth1Raw(lambda, phi) {
  const q = phi * phi, f = q * q;
  return [
    lambda * (0.8707 - 0.131979 * q + f * (-0.013791 + f * (0.003971 * q - 0.001529 * f))),
    phi * (1.007226 + q * (0.015085 + f * (-0.044475 + 0.028874 * q - 0.005916 * f))),
  ];
}
const scale = projection.scale();
const [tx, ty] = projection.translate();
const hand = (lon, lat) => {
  const p = naturalEarth1Raw((lon * Math.PI) / 180, (lat * Math.PI) / 180);
  return [tx + scale * p[0], ty - scale * p[1]];
};
const CITIES = [
  ['Beijing', 116.4, 39.9], ['London', -0.13, 51.5], ['New York', -74.0, 40.7],
  ['Singapore', 103.85, 1.35], ['Sydney', 151.2, -33.9], ['Sao Paulo', -46.6, -23.5],
  ['Reykjavik', -21.9, 64.1], ['Cape Town', 18.4, -33.9], ['Wellington', 174.8, -41.3],
];
let worst = 0;
for (const [name, lon, lat] of CITIES) {
  const a = projection([lon, lat]);
  const b = hand(lon, lat);
  const d = Math.max(Math.abs(a[0] - b[0]), Math.abs(a[1] - b[1]));
  worst = Math.max(worst, d);
  console.log(`  ${name.padEnd(10)} d3 ${a.map((v) => v.toFixed(2)).join(', ')}   hand ${b.map((v) => v.toFixed(2)).join(', ')}`);
}
console.log(`max |d3 - hand| over ${CITIES.length} cities = ${worst.toFixed(6)} px`);
if (worst > 0.01) {
  console.error('the hand formula no longer matches d3; the console would place devices off-country');
  process.exit(1);
}

// ------------------------------------------------------------------- output
fs.mkdirSync(OUT, { recursive: true });
const header = (what) => `// ${what}\n` +
  `// Geometry: Natural Earth (public domain), taken from the world-atlas topojson\n` +
  `// package (ISC) and projected with d3-geo geoNaturalEarth1 (ISC).\n` +
  `// Generator: controller/tools/world-basemap/gen.mjs; the input bytes it was\n` +
  `// regenerated from are pinned in tools/world-basemap/SHA256SUMS.\n` +
  `// Viewport ${W}x${H}, latitude crop >= ${LAT_MIN_DEG} deg, 0.1 px precision.\n` +
  `// Device points use the same projection in JS, verified against d3 to under\n` +
  `// 0.01 px, so a marker lands on its own landmass.\n`;

const write = (file, globalName, body) => {
  const text = header(globalName) + `window.${globalName}=${JSON.stringify(body)};\n`;
  fs.writeFileSync(path.join(OUT, file), text);
  const gz = zlib.gzipSync(Buffer.from(text)).length;
  console.log(`  ${file}: ${text.length} B (gz ${gz} B)`);
};

write('world-110m.js', 'SR_WORLD', {
  w: W, h: H, scale: +scale.toFixed(6), tx: +tx.toFixed(3), ty: +ty.toFixed(3),
  latMinDeg: LAT_MIN_DEG, ...layers110,
});
// The detail tier carries no projection parameters: it is drawn in the frame the
// 110m file defines, which is what keeps the two layers registered.
write('world-50m.js', 'SR_WORLD_50M', layers50);

const bounds = (geometry) => geoPath.bounds(feature(geometry)).map((p) => p.map((v) => Math.round(v)));
console.log('  projected bounds 110m land', JSON.stringify(bounds(landGeo110)));
console.log('  projected bounds  50m land', JSON.stringify(bounds(landGeo50)));
