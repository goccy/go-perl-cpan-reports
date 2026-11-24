// What the cpanfile checker (check.html) decides, apart from the page: it
// reads a cpanfile and a cpanfile.snapshot as text, compares versions the
// way version.pm does, and judges the requirements against check.json. It
// runs no Perl: a cpanfile is read by its usual forms only, and every line
// that is not one of them is reported, never guessed at.
//
// It defines CPANfileCheck on the global object, for check.js and the
// tests (site_test.go runs them in a JavaScript engine).
(function (global) {
  "use strict";

  // ---- versions (the same grammar as version.go) ----

  // parseVersion returns the components version.pm compares a version by,
  // or null when the text is not a version: a dotted-decimal version
  // ("v1.2.3", or any version with two or more dots) lists them directly; a
  // decimal version ("1.302") stands for its fraction cut into groups of
  // three digits ("1.3" is 1.300). Underscores do not change the value.
  function parseVersion(s) {
    s = String(s).trim().replace(/_/g, "");
    if (s === "") return null;
    var dotted = s.charAt(0) === "v";
    if (dotted) s = s.slice(1);
    var parts = s.split(".");
    if (parts.length > 2) dotted = true;
    var v = [];
    var i;
    if (dotted) {
      for (i = 0; i < parts.length; i++) {
        if (!/^[0-9]+$/.test(parts[i])) return null;
        v.push(parseInt(parts[i], 10));
      }
      return v;
    }
    var whole = parts[0] === "" ? "0" : parts[0];
    if (!/^[0-9]+$/.test(whole)) return null;
    v.push(parseInt(whole, 10));
    if (parts.length === 2) {
      var frac = parts[1];
      while (frac.length % 3 !== 0) frac += "0";
      for (i = 0; i < frac.length; i += 3) {
        var group = frac.slice(i, i + 3);
        if (!/^[0-9]{3}$/.test(group)) return null;
        v.push(parseInt(group, 10));
      }
    }
    return v;
  }

  // compareVersions returns -1, 0 or 1; missing components count as zero.
  function compareVersions(a, b) {
    for (var i = 0; i < a.length || i < b.length; i++) {
      var x = i < a.length ? a[i] : 0;
      var y = i < b.length ? b[i] : 0;
      if (x !== y) return x < y ? -1 : 1;
    }
    return 0;
  }

  var OPERATORS = [">=", "<=", "==", "!=", ">", "<"];

  // satisfies tells whether a module at version have meets the requirement
  // want, written as CPAN::Meta::Spec writes ranges: a bare version (at
  // least that), or clauses such as ">= 1.2, != 1.5, < 2.0". ok is false
  // when either side is not a version. A module without a version ("")
  // counts as version 0.
  function satisfies(have, want) {
    want = String(want).trim();
    if (want === "" || want === "0") return { satisfied: true, ok: true };
    var hv = [0];
    if (have !== "" && have !== null && have !== undefined) {
      hv = parseVersion(have);
      if (hv === null) return { satisfied: false, ok: false };
    }
    var clauses = want.split(",");
    for (var i = 0; i < clauses.length; i++) {
      var clause = clauses[i].trim();
      var op = ">=";
      for (var j = 0; j < OPERATORS.length; j++) {
        if (clause.indexOf(OPERATORS[j]) === 0) {
          op = OPERATORS[j];
          clause = clause.slice(op.length).trim();
          break;
        }
      }
      var wv = parseVersion(clause);
      if (wv === null) return { satisfied: false, ok: false };
      var c = compareVersions(hv, wv);
      var holds = { ">=": c >= 0, "<=": c <= 0, "==": c === 0, "!=": c !== 0, ">": c > 0, "<": c < 0 }[op];
      if (!holds) return { satisfied: false, ok: true };
    }
    return { satisfied: true, ok: true };
  }

  // ---- reading a cpanfile ----

  var PHASES = { configure: true, build: true, test: true, runtime: true, develop: true };
  var RELATIONSHIPS = { requires: true, recommends: true, suggests: true, conflicts: true };
  // The forms that name their phase.
  var PHASE_FORMS = {
    configure_requires: "configure", build_requires: "build", test_requires: "test", author_requires: "develop"
  };
  var BRACKETS = { "(": ")", "[": "]", "{": "}", "<": ">" };

  // tokenize cuts a cpanfile into tokens, each with its type, value and
  // line: str (a quoted string, or a v-string), num, word (a bareword, a
  // module name), the punctuation "=>" "," ";" "{" "}" "(" ")", and bad
  // (anything else: a string that interpolates, a character no form has).
  // Comments, POD and what follows __END__ or __DATA__ are not tokens.
  function tokenize(text) {
    var tokens = [];
    var i = 0;
    var line = 1;
    var n = text.length;
    function push(type, value, at) {
      tokens.push({ type: type, value: value, line: at });
    }
    function lineStart(pos) {
      return pos === 0 || text.charAt(pos - 1) === "\n";
    }
    // quoted reads a string from i, whose opening delimiter is open, and
    // returns its raw text (escapes kept), or null at the end of the text.
    function quoted(open) {
      var close = BRACKETS[open] || open;
      var depth = 1;
      var out = "";
      while (i < n) {
        var ch = text.charAt(i++);
        if (ch === "\n") line++;
        if (ch === "\\" && i < n) {
          var next = text.charAt(i++);
          if (next === "\n") line++;
          out += ch + next;
          continue;
        }
        if (ch === close && close !== open) {
          if (--depth === 0) return out;
        } else if (ch === close) {
          return out;
        } else if (ch === open && close !== open) {
          depth++;
        }
        out += ch;
      }
      return null;
    }
    // single unescapes a single-quoted string: only \\ and an escaped
    // delimiter are escapes.
    function single(raw, close) {
      return raw.replace(/\\([\\\s\S])/g, function (m, c) {
        return c === "\\" || c === close ? c : m;
      });
    }
    // double unescapes a double-quoted string, or returns null for one
    // that interpolates a variable (its value is not in the text).
    function dbl(raw) {
      var out = "";
      for (var k = 0; k < raw.length; k++) {
        var ch = raw.charAt(k);
        if (ch === "\\" && k + 1 < raw.length) {
          var e = raw.charAt(++k);
          out += e === "n" ? "\n" : e === "t" ? "\t" : e;
          continue;
        }
        if (ch === "$" || ch === "@") return null;
        out += ch;
      }
      return out;
    }
    while (i < n) {
      var ch = text.charAt(i);
      var at = line;
      if (ch === "\n") {
        line++;
        i++;
        continue;
      }
      if (/\s/.test(ch)) {
        i++;
        continue;
      }
      if (ch === "#") {
        while (i < n && text.charAt(i) !== "\n") i++;
        continue;
      }
      if (lineStart(i) && /^=[A-Za-z]/.test(text.slice(i, i + 2))) {
        // POD, up to the line after =cut.
        var cut = text.slice(i).search(/^=cut\b.*$/m);
        var end = cut < 0 ? n : i + cut;
        while (i < end) {
          if (text.charAt(i) === "\n") line++;
          i++;
        }
        while (i < n && text.charAt(i) !== "\n") i++;
        continue;
      }
      if (lineStart(i) && /^__(END|DATA)__\b/.test(text.slice(i, i + 9))) break;
      if (ch === "'" || ch === '"') {
        i++;
        var raw = quoted(ch);
        if (raw === null) {
          push("bad", ch, at);
          break;
        }
        var value = ch === "'" ? single(raw, "'") : dbl(raw);
        push(value === null ? "bad" : "str", value === null ? raw : value, at);
        continue;
      }
      if (text.slice(i, i + 2) === "=>") {
        push("fat", "=>", at);
        i += 2;
        continue;
      }
      if (",;{}()".indexOf(ch) >= 0) {
        push({ ",": "comma", ";": "semi", "{": "lbrace", "}": "rbrace", "(": "lparen", ")": "rparen" }[ch], ch, at);
        i++;
        continue;
      }
      var m = /^v[0-9]+(?:\.[0-9]+)*(?![A-Za-z0-9_])/.exec(text.slice(i));
      if (m) {
        push("str", m[0], at);
        i += m[0].length;
        continue;
      }
      m = /^[0-9][0-9_]*(?:\.[0-9_]*)?(?:[eE][+-]?[0-9]+)?/.exec(text.slice(i));
      if (m) {
        push("num", m[0].replace(/_/g, ""), at);
        i += m[0].length;
        continue;
      }
      m = /^[A-Za-z_][A-Za-z0-9_]*(?:::[A-Za-z0-9_]+)*/.exec(text.slice(i));
      if (m) {
        var word = m[0];
        i += word.length;
        if (word === "q" || word === "qq") {
          // A quote-like operator: q{...}, qq(...), q!...!; its delimiter
          // may follow white space.
          var j = i;
          while (j < n && /[ \t]/.test(text.charAt(j))) j++;
          var delim = text.charAt(j);
          if (j < n && !/[\sA-Za-z0-9_,;=]/.test(delim) && !(delim === "#" && j > i)) {
            i = j + 1;
            var body = quoted(delim);
            if (body === null) {
              push("bad", word, at);
              break;
            }
            var v = word === "q" ? single(body, BRACKETS[delim] || delim) : dbl(body);
            push(v === null ? "bad" : "str", v === null ? body : v, at);
            continue;
          }
        }
        // qw, and q or qq whose delimiter is not on its line (it follows a
        // comment), are not read.
        if (word === "qw" || word === "q" || word === "qq") {
          push("bad", word, at);
          continue;
        }
        push("word", word, at);
        continue;
      }
      push("bad", ch, at);
      i++;
    }
    return tokens;
  }

  // parseCPANfile reads the statements of a cpanfile: requires, recommends,
  // suggests and conflicts (with a module, a version and options), the
  // forms that name their phase (test_requires, ...), on '<phase>' => sub
  // { ... } and feature '<id>', '<description>' => sub { ... }, nested as
  // they are written. It returns the requirements, each with its module,
  // version range ("0" for any), phase, relationship, feature ("" outside
  // one) and line, and the statements it could not read, each with its
  // first and last line and its text.
  function parseCPANfile(text) {
    var tokens = tokenize(text);
    var lines = text.split("\n");
    var requirements = [];
    var unparsed = [];
    var pos = 0;

    function peek() {
      return pos < tokens.length ? tokens[pos] : null;
    }
    // skipStatement passes the rest of a statement that cannot be read: up
    // to its ";", or the "}" that closes the block it is in, or the end.
    function skipStatement(start) {
      var depth = 0;
      var last = start;
      while (pos < tokens.length) {
        var t = tokens[pos];
        if (t.type === "rbrace" && depth === 0) break;
        pos++;
        last = t.line;
        if (t.type === "lbrace" || t.type === "lparen") depth++;
        if (t.type === "rbrace" || t.type === "rparen") depth--;
        if (t.type === "semi" && depth === 0) break;
      }
      unparsed.push({ line: start, end: last, text: lines.slice(start - 1, last).join("\n") });
    }
    // values reads the comma-separated values of a statement up to its ";"
    // (consumed) or the "}" of its block (left), or returns null when one
    // is not a plain value. A comma may end the list, as in Perl.
    function values() {
      var out = [];
      var expectValue = true;
      while (pos < tokens.length) {
        var t = tokens[pos];
        if (t.type === "semi") {
          pos++;
          return out;
        }
        if (t.type === "rbrace") return out;
        if (expectValue) {
          if (t.type !== "str" && t.type !== "num" && t.type !== "word") return null;
          out.push(t);
        } else if (t.type !== "comma" && t.type !== "fat") {
          return null;
        }
        expectValue = !expectValue;
        pos++;
      }
      return out;
    }
    // block reads the statements of a sub { ... } body or of the file.
    function block(phase, feature, nested) {
      while (pos < tokens.length) {
        var t = tokens[pos];
        if (t.type === "rbrace") {
          if (nested) {
            pos++;
            return true;
          }
          skipStatement(t.line);
          pos++;
          continue;
        }
        if (t.type === "semi") {
          pos++;
          continue;
        }
        var start = t.line;
        var save = pos;
        if (!statement(t, phase, feature)) {
          pos = save;
          skipStatement(start);
        }
      }
      return !nested;
    }
    // subBlock reads "sub { ... }" and an optional ";" after it.
    function subBlock(phase, feature) {
      var s = peek();
      if (!s || s.type !== "word" || s.value !== "sub") return false;
      pos++;
      var b = peek();
      if (!b || b.type !== "lbrace") return false;
      pos++;
      if (!block(phase, feature, true)) return false;
      var after = peek();
      if (after && after.type === "semi") pos++;
      return true;
    }
    function separator() {
      var t = peek();
      if (t && (t.type === "comma" || t.type === "fat")) {
        pos++;
        return true;
      }
      return false;
    }
    function name() {
      var t = peek();
      if (t && (t.type === "str" || t.type === "word")) {
        pos++;
        return t.value;
      }
      return null;
    }
    function statement(t, phase, feature) {
      if (t.type !== "word") return false;
      var relationship = null;
      var stmtPhase = phase;
      if (RELATIONSHIPS[t.value]) {
        relationship = t.value;
      } else if (PHASE_FORMS[t.value]) {
        relationship = "requires";
        stmtPhase = PHASE_FORMS[t.value];
      }
      if (relationship) {
        pos++;
        var args = values();
        // A module, then a version, then option pairs (dist => ..., ...).
        if (args === null || args.length === 0 || args[0].type === "num") return false;
        if (args.length > 2 && (args.length - 2) % 2 !== 0) return false;
        if (args.length >= 2 && args[1].type === "word") return false;
        requirements.push({
          module: args[0].value, version: args.length >= 2 ? args[1].value : "0", phase: stmtPhase,
          relationship: relationship, feature: feature, line: t.line
        });
        return true;
      }
      if (t.value === "on") {
        pos++;
        var p = name();
        if (p === null || !PHASES[p] || !separator()) return false;
        return subBlock(p, feature);
      }
      if (t.value === "feature") {
        pos++;
        var id = name();
        if (id === null || !separator()) return false;
        var d = peek();
        if (d && d.type === "str") {
          pos++;
          if (!separator()) return false;
        }
        return subBlock(phase, id);
      }
      return false;
    }

    block("runtime", "", false);
    return { requirements: requirements, unparsed: unparsed };
  }

  // parseSnapshot reads a carton snapshot: its distributions, each with its
  // name, pathname and the packages it provides, nested by indentation. It
  // returns them, and the lines it could not read.
  function parseSnapshot(text) {
    var dists = [];
    var errors = [];
    var dist = null;
    var list = null;
    text.split("\n").forEach(function (raw, k) {
      var line = raw.replace(/\r$/, "");
      var body = line.replace(/^ +/, "");
      var indent = line.length - body.length;
      if (body === "" || body.charAt(0) === "#" || body === "DISTRIBUTIONS") return;
      if (indent === 2) {
        dist = { name: body, pathname: "", provides: [], requirements: [], line: k + 1 };
        dists.push(dist);
        list = null;
      } else if (indent === 4 && dist) {
        var m = /^([a-z_]+):\s*(.*)$/.exec(body);
        if (!m) {
          errors.push({ line: k + 1, end: k + 1, text: line });
        } else if (m[1] === "pathname") {
          dist.pathname = m[2];
          list = null;
        } else if (m[1] === "provides" || m[1] === "requirements") {
          list = dist[m[1]];
        } else {
          list = null;
        }
      } else if (indent === 6 && list) {
        var parts = body.split(/\s+/);
        list.push([parts[0], parts.length > 1 && parts[1] !== "undef" ? parts[1] : ""]);
      } else {
        errors.push({ line: k + 1, end: k + 1, text: line });
      }
    });
    return { distributions: dists, errors: errors };
  }

  // isSnapshot tells a snapshot from a cpanfile by its first line.
  function isSnapshot(text) {
    return /^# carton snapshot format/.test(text);
  }

  // ---- judging ----

  // The verdicts.
  var YES = "yes", NO = "no", UNKNOWN = "unknown";

  // Index is check.json, with what the judging looks up.
  function Index(data) {
    this.data = data;
    var col = {};
    data.columns.forEach(function (c, i) { col[c] = i; });
    this.col = col;
    this.byRelease = {};
    for (var i = 0; i < data.dists.length; i++) this.byRelease[data.dists[i][col.release]] = i;
  }

  // module finds a module of the package index: [name, dist, version], or
  // null. The modules are sorted by name.
  Index.prototype.module = function (name) {
    var m = this.data.modules;
    var lo = 0, hi = m.length - 1;
    while (lo <= hi) {
      var mid = (lo + hi) >> 1;
      var c = m[mid][0] < name ? -1 : m[mid][0] > name ? 1 : 0;
      if (c === 0) return m[mid];
      if (c < 0) lo = mid + 1; else hi = mid - 1;
    }
    return null;
  };

  Index.prototype.dist = function (i) {
    var d = this.data.dists[i];
    var c = this.col;
    return {
      index: i, name: d[c.dist], release: d[c.release], outcome: d[c.outcome] < 0 ? "" : this.data.outcomes[d[c.outcome]],
      static: d[c.static] < 0 ? null : this.data.static[d[c.static]], libraries: d[c.libraries], page: d[c.page],
      needs: d[c.needs], closure: d[c.closure]
    };
  };

  // closure returns the indexes of the distributions dist needs, directly
  // or indirectly: the ones its program carried when its static build was
  // measured (what the configure steps asked for), or else the ones its
  // metadata names.
  Index.prototype.closure = function (dist) {
    if (dist.closure.length > 0) return dist.closure.filter(function (j) { return j !== dist.index; });
    var seen = {};
    seen[dist.index] = true;
    var stack = [dist.index];
    var out = [];
    while (stack.length > 0) {
      var needs = this.dist(stack.pop()).needs;
      for (var k = 0; k < needs.length; k++) {
        if (!seen[needs[k]]) {
          seen[needs[k]] = true;
          out.push(needs[k]);
          stack.push(needs[k]);
        }
      }
    }
    return out;
  };

  // judgeDist judges a distribution of the survey: whether it is supported
  // (it and everything it needs passed their tests) and static (its static
  // build), with the notes that say why not.
  function judgeDist(index, dist, withClosure) {
    var r = { dist: dist, supported: YES, static: UNKNOWN, notes: [], libraries: dist.libraries };
    if (dist.outcome === "") {
      r.supported = UNKNOWN;
      r.notes.push("the survey did not run " + dist.name);
    } else if (dist.outcome !== "pass") {
      r.supported = NO;
      r.notes.push(dist.name + ": " + dist.outcome);
    } else if (withClosure) {
      index.closure(dist).forEach(function (j) {
        var d = index.dist(j);
        if (d.outcome === "pass") return;
        if (d.outcome === "") {
          if (r.supported === YES) r.supported = UNKNOWN;
          r.notes.push("it needs " + d.name + ", which the survey did not run");
        } else {
          r.supported = NO;
          r.notes.push("it needs " + d.name + ": " + d.outcome);
        }
      });
    }
    if (dist.static === null) {
      r.notes.push("its static build was not measured");
    } else if (dist.static.id.indexOf("static") === 0) {
      r.static = YES;
    } else if (dist.static.id === "driver_failed") {
      r.notes.push("its static build was not measured");
    } else {
      r.static = NO;
      r.notes.push(dist.static.title);
    }
    return r;
  }

  // bundled is the verdict of a requirement the interpreter meets itself.
  function bundled(note) {
    return { dist: null, supported: YES, static: YES, notes: [note], libraries: [] };
  }

  function unknown(note) {
    return { dist: null, supported: UNKNOWN, static: UNKNOWN, notes: [note], libraries: [] };
  }

  // judgeModule judges one requirement: met by perl or the interpreter's
  // standard library, or by the distribution the package index gives,
  // whose indexed version must be in the range.
  function judgeModule(index, module, range) {
    var data = index.data;
    if (module === "perl") {
      var p = satisfies(data.perl, range);
      if (!p.ok) return unknown("the range " + range + " is not a version range");
      return p.satisfied ? bundled("perl " + data.perl) :
        { dist: null, supported: NO, static: NO, notes: ["the interpreter is perl " + data.perl], libraries: [] };
    }
    if (Object.prototype.hasOwnProperty.call(data.stdlib, module)) {
      var have = data.stdlib[module] === null ? "" : data.stdlib[module];
      var s = satisfies(have, range);
      if (s.ok && s.satisfied) return bundled("the interpreter ships " + module + (have ? " " + have : ""));
    }
    var m = index.module(module);
    if (m === null) return unknown("the package index of the snapshot does not know " + module);
    var v = satisfies(m[2], range);
    if (!v.ok) return unknown("the version " + (m[2] || "(none)") + " cannot be compared with " + range);
    var dist = index.dist(m[1]);
    if (!v.satisfied) {
      var r = unknown("the survey measured " + dist.release + ", whose " + module + " is " + (m[2] || "without a version") +
        ", outside " + range);
      r.dist = dist;
      return r;
    }
    return judgeDist(index, dist, true);
  }

  // judgeRelease judges a distribution a snapshot pins: the survey's
  // results hold for the release it measured only.
  function judgeRelease(index, d) {
    var i = index.byRelease[d.name];
    if (i !== undefined) return judgeDist(index, index.dist(i), false);
    for (var k = 0; k < d.provides.length; k++) {
      var m = index.module(d.provides[k][0]);
      if (m !== null) {
        var r = unknown("the survey measured " + index.dist(m[1]).release + ", not " + d.name);
        r.dist = index.dist(m[1]);
        return r;
      }
    }
    return unknown("the snapshot of the survey has no release of " + d.name);
  }

  // overall combines verdicts: no when one is no, else unknown when one
  // is unknown, else yes.
  function overall(rows, key) {
    var v = YES;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].verdict[key] === NO) return NO;
      if (rows[i].verdict[key] === UNKNOWN) v = UNKNOWN;
    }
    return v;
  }

  // check judges a cpanfile and a snapshot (either may be empty) against
  // check.json; a statement it could not read makes a verdict of yes
  // unknown. Without a snapshot, the requirements the cpanfile counts
  // (requires of every phase but develop, outside features) are judged, each
  // with what it needs; with one, the distributions it pins are, and each
  // requirement is shown with the distribution of the snapshot that
  // provides it. The other declarations are listed apart.
  function check(data, cpanfileText, snapshotText) {
    var index = new Index(data);
    var out = { rows: [], requirements: [], others: [], unparsed: [], supported: YES, static: YES, libraries: [] };
    var counted = [];
    if (cpanfileText.trim() !== "") {
      var parsed = parseCPANfile(cpanfileText);
      out.unparsed = parsed.unparsed;
      var byModule = {};
      parsed.requirements.forEach(function (r) {
        if (r.relationship !== "requires" || r.phase === "develop" || r.feature !== "") {
          out.others.push(r);
          return;
        }
        var c = byModule[r.module];
        if (!c) {
          c = byModule[r.module] = { module: r.module, ranges: [], phases: [], lines: [] };
          counted.push(c);
        }
        if (r.version !== "0" && c.ranges.indexOf(r.version) < 0) c.ranges.push(r.version);
        if (c.phases.indexOf(r.phase) < 0) c.phases.push(r.phase);
        c.lines.push(r.line);
      });
    }
    var snap = snapshotText.trim() === "" ? null : parseSnapshot(snapshotText);
    if (snap) {
      out.unparsed = out.unparsed.concat(snap.errors);
      var provider = {};
      snap.distributions.forEach(function (d) {
        var row = { kind: "release", name: d.name, verdict: judgeRelease(index, d) };
        out.rows.push(row);
        d.provides.forEach(function (p) {
          if (!provider[p[0]]) provider[p[0]] = { row: row, version: p[1] };
        });
      });
      counted.forEach(function (c) {
        var range = c.ranges.length ? c.ranges.join(", ") : "0";
        var p = provider[c.module];
        var verdict;
        if (p) {
          var s = satisfies(p.version, range);
          verdict = s.ok && s.satisfied ? p.row.verdict :
            unknown("the snapshot pins " + p.row.name + ", whose " + c.module + " is not in " + range);
        } else {
          verdict = judgeModule(index, c.module, range);
          if (verdict.dist !== null) {
            verdict = unknown("the snapshot does not pin " + c.module);
          }
        }
        var row = { kind: "requirement", name: c.module, range: range, phases: c.phases, lines: c.lines, verdict: verdict };
        out.requirements.push(row);
        if (!p) out.rows.push(row);
      });
    } else {
      counted.forEach(function (c) {
        var range = c.ranges.length ? c.ranges.join(", ") : "0";
        var row = {
          kind: "requirement", name: c.module, range: range, phases: c.phases, lines: c.lines,
          verdict: judgeModule(index, c.module, range)
        };
        out.requirements.push(row);
        out.rows.push(row);
      });
    }
    out.supported = overall(out.rows, "supported");
    out.static = overall(out.rows, "static");
    // What could not be read may require anything.
    if (out.unparsed.length > 0) {
      if (out.supported === YES) out.supported = UNKNOWN;
      if (out.static === YES) out.static = UNKNOWN;
    }
    out.rows.forEach(function (r) {
      r.verdict.libraries.forEach(function (l) {
        if (out.libraries.indexOf(l) < 0) out.libraries.push(l);
      });
    });
    out.libraries.sort();
    return out;
  }

  global.CPANfileCheck = {
    parseVersion: parseVersion,
    compareVersions: compareVersions,
    satisfies: satisfies,
    tokenize: tokenize,
    parseCPANfile: parseCPANfile,
    parseSnapshot: parseSnapshot,
    isSnapshot: isSnapshot,
    check: check,
    YES: YES,
    NO: NO,
    UNKNOWN: UNKNOWN
  };
})(this);
