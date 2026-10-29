// #118 规模预演的 mock 数据发生器：注入到真实页面里，喂给 /api/underlay/topology。
// 只存在于浏览器内存，不进控制器、不进仓库数据。用法见同目录 README.md。
() => {
  var cities = [
    ["北京·国贸", 116.46, 39.91], ["上海·张江", 121.6, 31.2], ["深圳·南山", 113.93, 22.53],
    ["广州·珠江新城", 113.26, 23.13], ["杭州·滨江", 120.16, 30.27], ["成都·天府", 104.07, 30.57],
    ["新加坡·裕廊", 103.82, 1.35], ["东京·品川", 139.69, 35.69], ["首尔·江南", 126.98, 37.57],
    ["孟买·BKC", 72.88, 19.08], ["班加罗尔", 77.6, 12.97], ["迪拜·Internet City", 55.27, 25.2],
    ["特拉维夫", 34.87, 32.08], ["雅加达", 106.85, -6.21], ["曼谷", 100.5, 13.76],
    ["马尼拉", 121.0, 14.6], ["悉尼", 151.2, -33.87], ["墨尔本", 144.96, -37.81],
    ["奥克兰", 174.76, -36.85], ["伦敦·Docklands", -0.13, 51.51], ["法兰克福", 8.68, 50.11],
    ["阿姆斯特丹", 4.9, 52.37], ["巴黎", 2.35, 48.86], ["马德里", -3.7, 40.42],
    ["米兰", 9.19, 45.46], ["华沙", 21.01, 52.23], ["斯德哥尔摩", 18.06, 59.33],
    ["都柏林", -6.26, 53.35], ["伊斯坦布尔", 28.98, 41.01], ["苏黎世", 8.54, 47.37],
    ["里斯本", -9.14, 38.72], ["雷克雅未克", -21.94, 64.15], ["莫斯科", 37.62, 55.76],
    ["开普敦", 18.42, -33.93], ["约翰内斯堡", 28.04, -26.2], ["内罗毕", 36.82, -1.29],
    ["拉各斯", 3.38, 6.52], ["开罗", 31.24, 30.04], ["卡萨布兰卡", -7.59, 33.57],
    ["纽约·Manhattan", -74.01, 40.71], ["旧金山", -122.42, 37.77], ["洛杉矶", -118.24, 34.05],
    ["芝加哥", -87.63, 41.88], ["达拉斯", -96.8, 32.78], ["亚特兰大", -84.39, 33.75],
    ["多伦多", -79.38, 43.65], ["温哥华", -123.12, 49.28], ["墨西哥城", -99.13, 19.43],
    ["圣保罗", -46.63, -23.55], ["布宜诺斯艾利斯", -58.38, -34.6], ["波哥大", -74.07, 4.71],
    ["圣地亚哥", -70.67, -33.45]
  ];

  function rng(seed) {
    return function () {
      seed |= 0; seed = seed + 0x6D2B79F5 | 0;
      var t = Math.imul(seed ^ seed >>> 15, 1 | seed);
      t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
      return ((t ^ t >>> 14) >>> 0) / 4294967296;
    };
  }

  window.__gen118 = function (N) {
    var r = rng(20261029);
    var now = Math.floor(Date.now() / 1000);
    var nodes = [], sites = [], siteById = {}, perCity = {};
    var hubA = 1, hubB = 2;

    function mkSite(id, name, lon, lat) {
      var s = { id: id, name: name, lon: lon, lat: lat, devices: 0, created_at_unix: now - 4000 };
      sites.push(s);
      siteById[id] = s;
      return s;
    }

    function mkNode(id, serial, siteId, siteName, lon, lat) {
      var roll = r();
      return {
        node_id: id, serial: serial, agent_version: "sr-agent/0.1 (identity+heartbeat)",
        status: roll < 0.9 ? "online" : (roll < 0.95 ? "stale" : "pending"),
        last_seen_unix: now - Math.floor(r() * 600), agent_uptime_sec: 0,
        observed_addr: "203.0.113." + (id % 250), udp_mapping: "preserved",
        observed_age_sec: Math.floor(r() * 30), observed_stale: false,
        symmetric_nat: r() < 0.08, inventory_reported: true, inventory_age_sec: 3,
        ifaces: [{ name: "host-eth1", sw_if_index: 1, mtu: 1500, admin_up: true, link_up: true }],
        default_iface: "host-eth1", transit_port: 15000 + (id % 900), ports: [],
        tunnel_count: 0, relay_for: 0,
        site_id: siteId || "", site_name: siteName || "", lon: lon || 0, lat: lat || 0,
        has_location: !!siteId,
        tunnel_delivery: { capable: true, version: 8, applying_version: 8, ok: true, applied_at_unix: now - 900, tunnels: 0 }
      };
    }

    mkSite("site_hub_a", "北京·国贸·HUB", 116.46, 39.91).devices++;
    mkSite("site_hub_b", "法兰克福·HUB", 8.68, 50.11).devices++;
    nodes.push(mkNode(hubA, "HUB-北京", "site_hub_a", "北京·国贸·HUB", 116.46, 39.91));
    nodes.push(mkNode(hubB, "HUB-法兰克福", "site_hub_b", "法兰克福·HUB", 8.68, 50.11));

    for (var i = 0; i < N - 2; i++) {
      var id = i + 3;
      var c = Math.floor(r() * cities.length);
      var ci = cities[c];
      var unplaced = r() < 0.05;
      var siteId = "", siteName = "", lon = 0, lat = 0;
      if (!unplaced) {
        if (!perCity[c]) perCity[c] = { group: -1, left: 0 };
        if (perCity[c].left <= 0) {
          var roll = r();
          perCity[c].left = roll < 0.7 ? 1 : (roll < 0.9 ? 2 + Math.floor(r() * 2) : 4 + Math.floor(r() * 5));
          perCity[c].group++;
          mkSite("site_c" + c + "_" + perCity[c].group, ci[0] + " #" + (perCity[c].group + 1), ci[1], ci[2]);
        }
        perCity[c].left--;
        siteId = "site_c" + c + "_" + perCity[c].group;
        siteName = ci[0] + " #" + (perCity[c].group + 1);
        siteById[siteId].devices++;
        lon = ci[1] + (r() - 0.5) * 0.02;
        lat = ci[2] + (r() - 0.5) * 0.02;
      }
      nodes.push(mkNode(id, "CPE-" + ("0000" + id).slice(-4), siteId, siteName, Math.round(lon * 1e4) / 1e4, Math.round(lat * 1e4) / 1e4));
    }

    var bundles = [], tseq = 0;
    function addBundle(a, b, relayed, broken) {
      var na = nodes[a - 1], nb = nodes[b - 1];
      var p = 20000 + (++tseq);
      var t = {
        id: "t" + tseq,
        a: { node_id: a, serial: na.serial, iface: "host-eth1", addr: "10.88.1." + (a % 200), port: p, overlay: "10.250.0." + (a % 200) },
        b: { node_id: b, serial: nb.serial, iface: "host-eth1", addr: "10.88.1." + (b % 200), port: p + 1, overlay: "10.250.0." + (b % 200) },
        mtu: 1368, mtu_is_default: false, roles: {},
        created_at_unix: now - 8000, updated_at_unix: now - 800,
        state: r() < 0.85 ? "up" : "down"
      };
      if (relayed) {
        var h = a === hubA ? hubB : hubA;
        t.path = { node_id: h, serial: nodes[h - 1].serial, addr: "10.88.1." + h };
        nodes[h - 1].relay_for++;
      }
      if (broken) t.path_error = "路径模板已删除";
      na.tunnel_count++; nb.tunnel_count++;
      na.tunnel_delivery.tunnels++; nb.tunnel_delivery.tunnels++;
      bundles.push({
        a: a, b: b, serial_a: na.serial, serial_b: nb.serial, count: 1,
        state: t.state === "up" ? "up" : "unknown", tunnels: [t]
      });
    }

    addBundle(hubA, hubB, false, false);
    for (var k = 3; k <= N; k++) {
      if (!nodes[k - 1].has_location && r() < 0.5) continue;
      addBundle(hubA, k, false, r() < 0.01);
      if (r() < 0.3) addBundle(hubB, k, r() < 0.35, false);
    }
    for (var m = 0; m < Math.floor(N / 6); m++) {
      var x = 3 + Math.floor(r() * (N - 3)), y = 3 + Math.floor(r() * (N - 3));
      if (x !== y) addBundle(x, y, r() < 0.5, false);
    }

    var unplacedCount = nodes.filter(function (n) { return !n.has_location; }).length;
    return {
      now_unix: now, configured: true, state_note: "",
      nodes: nodes, bundles: bundles, sites: sites, unplaced_nodes: unplacedCount
    };
  };

  window.__mock118 = function (N) {
    var t0 = performance.now();
    var doc = window.__gen118(N);
    var genMs = performance.now() - t0;
    var json = JSON.stringify(doc);
    var p0 = performance.now();
    window.__doc118 = JSON.parse(json);
    var parseMs = performance.now() - p0;
    if (!window.__of118) window.__of118 = window.fetch.bind(window);
    window.fetch = function (u, o) {
      if (String(u).indexOf("/api/underlay/topology") >= 0) {
        return Promise.resolve(new Response(json,
          { status: 200, headers: { "content-type": "application/json" } }));
      }
      return window.__of118(u, o);
    };
    // 自检：mock 自己也得对得上，否则量到的数字是 mock 的 bug 不是页面的。
    var byId = {};
    doc.sites.forEach(function (s) { byId[s.id] = s; });
    var placed = 0, dangling = 0, sumDev = 0;
    doc.nodes.forEach(function (n) {
      if (n.has_location) { placed++; if (!byId[n.site_id]) dangling++; }
    });
    doc.sites.forEach(function (s) { sumDev += s.devices; });
    return { nodes: doc.nodes.length, bundles: doc.bundles.length, sites: doc.sites.length,
      unplaced: doc.unplaced_nodes, jsonKB: Math.round(json.length / 1024),
      genMs: Math.round(genMs * 10) / 10, parseMs: Math.round(parseMs * 10) / 10,
      check: { placed: placed, sumSiteDevices: sumDev, danglingSiteRefs: dangling,
        unplacedMatches: doc.unplaced_nodes === doc.nodes.length - placed } };
  };
  return "generator installed: " + cities.length + " cities";
}
