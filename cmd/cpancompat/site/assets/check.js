// The cpanfile checker page: reads check.json, takes a cpanfile and a
// cpanfile.snapshot from the text areas or from files (read in the browser,
// sent nowhere), and shows what CPANfileCheck (cpanfile.js) decides. Every
// value goes into the page as text (textContent), never as markup.
(function () {
  "use strict";
  var C = window.CPANfileCheck;
  var form = document.getElementById("check");
  var status = document.getElementById("status");
  var results = document.getElementById("results");
  var data = null;

  function el(tag, text, className) {
    var e = document.createElement(tag);
    if (text !== undefined && text !== null) e.textContent = text;
    if (className) e.className = className;
    return e;
  }

  var WORDS = {};
  WORDS[C.YES] = "yes";
  WORDS[C.NO] = "no";
  WORDS[C.UNKNOWN] = "cannot tell";

  function verdictCell(tr, v) {
    tr.appendChild(el("td", WORDS[v], "outcome v-" + v));
  }

  function distLink(dist) {
    if (!dist) return el("span", "");
    if (!dist.page) return el("span", dist.release);
    var a = el("a", dist.release);
    a.href = "d/" + encodeURIComponent(dist.page) + ".html";
    return a;
  }

  function render(out) {
    results.textContent = "";
    if (out.rows.length === 0 && out.unparsed.length === 0 && out.others.length === 0) {
      status.textContent = "Nothing to check: give a cpanfile or a cpanfile.snapshot.";
      return;
    }
    status.textContent = "";
    var summary = el("dl", null, "facts");
    summary.appendChild(el("dt", "supported"));
    var dd = el("dd");
    dd.appendChild(el("span", WORDS[out.supported], "outcome v-" + out.supported));
    dd.appendChild(document.createTextNode(" every distribution, and everything it needs, passed its tests on " + data.subject));
    summary.appendChild(dd);
    summary.appendChild(el("dt", "static build"));
    dd = el("dd");
    dd.appendChild(el("span", WORDS[out.static], "outcome v-" + out.static));
    dd.appendChild(document.createTextNode(out.libraries.length ?
      " links these libraries of the system from their static archives: " + out.libraries.map(function (l) { return "lib" + l; }).join(", ") :
      " needs no library of the system"));
    summary.appendChild(dd);
    results.appendChild(summary);
    results.appendChild(el("p", "Each distribution's static build was measured with what it needs, not together " +
      "with the others; a binary of all of them was not built.", "muted"));

    if (out.unparsed.length) {
      results.appendChild(el("h2", "Lines not read"));
      results.appendChild(el("p", "These statements are not one of the forms the checker reads; what they declare is " +
        "not in the verdicts."));
      var pre = el("pre", null, "log");
      out.unparsed.forEach(function (u) {
        pre.appendChild(document.createTextNode((u.line === u.end ? "line " + u.line : "lines " + u.line + "-" + u.end) +
          ":\n" + u.text + "\n"));
      });
      results.appendChild(pre);
    }

    results.appendChild(el("h2", "Requirements"));
    var wrap = el("div", null, "table-wrap");
    var table = el("table");
    var head = el("tr");
    ["requirement", "distribution", "tests on " + data.subject, "supported", "static build", "notes"].forEach(function (h) {
      head.appendChild(el("th", h));
    });
    table.appendChild(el("thead")).appendChild(head);
    var body = el("tbody");
    out.rows.forEach(function (row) {
      var tr = el("tr");
      var name = row.kind === "release" ? row.name : row.name + (row.range !== "0" ? " " + row.range : "");
      var td = el("td");
      td.appendChild(el("code", name));
      if (row.phases) td.appendChild(el("span", " " + row.phases.join(", "), "muted"));
      tr.appendChild(td);
      td = el("td");
      td.appendChild(distLink(row.verdict.dist));
      tr.appendChild(td);
      tr.appendChild(el("td", row.verdict.dist ? row.verdict.dist.outcome || "no record" : ""));
      verdictCell(tr, row.verdict.supported);
      verdictCell(tr, row.verdict.static);
      tr.appendChild(el("td", row.verdict.notes.join("; ")));
      body.appendChild(tr);
    });
    table.appendChild(body);
    wrap.appendChild(table);
    results.appendChild(wrap);

    if (out.requirements.length && out.rows.some(function (r) { return r.kind === "release"; })) {
      results.appendChild(el("h2", "The cpanfile's requirements in the snapshot"));
      var ul = el("ul");
      out.requirements.forEach(function (r) {
        ul.appendChild(el("li", r.name + (r.range !== "0" ? " " + r.range : "") + ": " +
          (r.verdict.dist ? r.verdict.dist.release : r.verdict.notes.join("; "))));
      });
      results.appendChild(ul);
    }

    if (out.others.length) {
      results.appendChild(el("h2", "Not counted"));
      results.appendChild(el("p", "Recommendations, suggestions, conflicts, development requirements and optional " +
        "features: gperl does not install them."));
      var list = el("ul");
      out.others.forEach(function (r) {
        list.appendChild(el("li", r.module + (r.version !== "0" ? " " + r.version : "") + ": " + r.relationship + ", " +
          r.phase + (r.feature ? ", feature " + r.feature : "") + " (line " + r.line + ")"));
      });
      results.appendChild(list);
    }
  }

  function run() {
    if (!data) return;
    render(C.check(data, form.elements.namedItem("cpanfile").value, form.elements.namedItem("snapshot").value));
  }

  // A chosen file goes to the area its first line says it belongs in.
  form.elements.namedItem("files").addEventListener("change", function (ev) {
    Array.prototype.forEach.call(ev.target.files, function (file) {
      var reader = new FileReader();
      reader.onload = function () {
        var text = String(reader.result);
        form.elements.namedItem(C.isSnapshot(text) ? "snapshot" : "cpanfile").value = text;
        run();
      };
      reader.onerror = function () {
        status.textContent = file.name + " could not be read: " + reader.error;
      };
      reader.readAsText(file);
    });
  });
  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    run();
  });

  fetch("check.json")
    .then(function (resp) {
      if (!resp.ok) throw new Error(resp.status + " " + resp.statusText);
      return resp.json();
    })
    .then(function (d) {
      data = d;
      status.textContent = "Ready: " + d.dists.length.toLocaleString("en-US") + " distributions and " +
        d.modules.length.toLocaleString("en-US") + " modules of the snapshot.";
      if (form.elements.namedItem("cpanfile").value || form.elements.namedItem("snapshot").value) run();
    })
    .catch(function (err) {
      status.textContent = "The data could not be loaded (" + err.message + "). Viewed from the file system, " +
        "the browser may refuse to read check.json: serve the directory instead (cpancompat site -serve).";
    });
})();
