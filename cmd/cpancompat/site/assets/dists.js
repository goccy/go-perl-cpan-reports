// The distributions table: reads dists.json, filters and sorts its rows in
// the browser, and renders them a page at a time. Every value goes into the
// page as text (textContent), never as markup.
(function () {
  "use strict";
  var PAGE = 200;
  var form = document.getElementById("filters");
  var body = document.querySelector("#dists tbody");
  var count = document.getElementById("count");
  var more = document.getElementById("more");
  var data = null;
  var shown = 0;
  var matches = [];

  function field(name) {
    return form.elements.namedItem(name);
  }

  function option(select, value, text) {
    var o = document.createElement("option");
    o.value = value;
    o.textContent = text;
    select.appendChild(o);
  }

  function fillSelects() {
    data.outcomes.forEach(function (o, i) {
      option(field("subject"), String(i), o);
      if (field("reference")) option(field("reference"), String(i), o);
    });
    option(field("subject"), "none", "no record");
    if (field("reference")) option(field("reference"), "none", "no record");
    data.causes.forEach(function (c, i) {
      option(field("cause"), String(i), c.title);
    });
    if (field("bucket")) {
      data.buckets.forEach(function (b, i) {
        option(field("bucket"), String(i), b.title);
      });
    }
    if (field("static")) {
      data.static.forEach(function (c, i) {
        option(field("static"), String(i), c.title);
      });
      option(field("static"), "none", "not measured");
    }
  }

  // The filters are kept in the address's fragment, by the names of their
  // values (outcome, cause and bucket names), so that a link can open the
  // table filtered.
  function readHash() {
    var params = new URLSearchParams(location.hash.slice(1));
    var q = params.get("q");
    if (q !== null) field("q").value = q;
    var order = params.get("order");
    if (order === "name") field("order").value = order;
    [["subject", data.outcomes], ["reference", data.outcomes]].forEach(function (pair) {
      var v = params.get(pair[0]);
      if (v === null || !field(pair[0])) return;
      field(pair[0]).value = v === "none" ? "none" : String(pair[1].indexOf(v));
    });
    [["cause", data.causes], ["bucket", data.buckets], ["static", data.static]].forEach(function (pair) {
      var v = params.get(pair[0]);
      if (v === null || !field(pair[0])) return;
      if (v === "none" && pair[0] === "static") {
        field(pair[0]).value = "none";
        return;
      }
      var i = pair[1].findIndex(function (x) { return x.id === v; });
      field(pair[0]).value = i < 0 ? "" : String(i);
    });
  }

  function writeHash() {
    var params = new URLSearchParams();
    var q = field("q").value.trim();
    if (q) params.set("q", q);
    ["subject", "reference"].forEach(function (name) {
      var f = field(name);
      if (!f || f.value === "") return;
      params.set(name, f.value === "none" ? "none" : data.outcomes[Number(f.value)]);
    });
    if (field("cause").value !== "") params.set("cause", data.causes[Number(field("cause").value)].id);
    if (field("bucket") && field("bucket").value !== "") params.set("bucket", data.buckets[Number(field("bucket").value)].id);
    if (field("static") && field("static").value !== "") {
      params.set("static", field("static").value === "none" ? "none" : data.static[Number(field("static").value)].id);
    }
    if (field("order").value === "name") params.set("order", "name");
    var s = params.toString();
    history.replaceState(null, "", s ? "#" + s : location.pathname + location.search);
  }

  function selected(name) {
    var f = field(name);
    if (!f || f.value === "") return null;
    return f.value === "none" ? -1 : Number(f.value);
  }

  function apply() {
    var q = field("q").value.trim().toLowerCase();
    var subject = selected("subject");
    var reference = selected("reference");
    var cause = selected("cause");
    var bucket = selected("bucket");
    var stat = selected("static");
    matches = data.rows.filter(function (r) {
      if (q && r[0].toLowerCase().indexOf(q) < 0) return false;
      if (subject !== null && r[3] !== subject) return false;
      if (reference !== null && r[4] !== reference) return false;
      if (cause !== null && r[5] !== cause) return false;
      if (bucket !== null && r[6] !== bucket) return false;
      if (stat !== null && r[8] !== stat) return false;
      return true;
    });
    if (field("order").value === "name") {
      matches.sort(function (a, b) { return a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0; });
    } else {
      matches.sort(function (a, b) { return b[2] - a[2] || (a[0] < b[0] ? -1 : 1); });
    }
    body.textContent = "";
    shown = 0;
    render();
  }

  function cell(tr, text, className) {
    var td = document.createElement("td");
    td.textContent = text;
    if (className) td.className = className;
    tr.appendChild(td);
    return td;
  }

  function outcomeCell(tr, i) {
    var td = cell(tr, i < 0 ? "no record" : data.outcomes[i], "outcome");
    if (i >= 0) td.classList.add("o-" + data.outcomes[i]);
  }

  function render() {
    var end = Math.min(matches.length, shown + PAGE);
    var frag = document.createDocumentFragment();
    for (var k = shown; k < end; k++) {
      var r = matches[k];
      var tr = document.createElement("tr");
      var td = document.createElement("td");
      var a = document.createElement("a");
      a.href = "d/" + encodeURIComponent(r[7] || r[0]) + ".html";
      a.textContent = r[0];
      td.appendChild(a);
      tr.appendChild(td);
      cell(tr, r[1], "muted");
      outcomeCell(tr, r[3]);
      if (field("reference")) outcomeCell(tr, r[4]);
      cell(tr, r[5] < 0 ? "" : data.causes[r[5]].title);
      if (field("static")) cell(tr, r[8] < 0 ? "" : data.static[r[8]].title);
      cell(tr, r[2].toLocaleString("en-US"), "num");
      frag.appendChild(tr);
    }
    body.appendChild(frag);
    shown = end;
    count.textContent = matches.length.toLocaleString("en-US") + " of " + data.rows.length.toLocaleString("en-US") +
      " distributions" + (matches.length > shown ? ", the first " + shown.toLocaleString("en-US") + " shown" : "");
    more.hidden = shown >= matches.length;
  }

  more.addEventListener("click", render);
  form.addEventListener("input", function () { writeHash(); apply(); });
  window.addEventListener("hashchange", function () { readHash(); apply(); });

  fetch("dists.json")
    .then(function (resp) {
      if (!resp.ok) throw new Error(resp.status + " " + resp.statusText);
      return resp.json();
    })
    .then(function (d) {
      data = d;
      fillSelects();
      readHash();
      apply();
    })
    .catch(function (err) {
      count.textContent = "The table could not be loaded (" + err.message + "). Viewed from the file system, " +
        "the browser may refuse to read dists.json: serve the directory instead (cpancompat site -serve).";
    });
})();
