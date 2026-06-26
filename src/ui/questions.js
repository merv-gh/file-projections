// questions.js — the Questions panel: plain-language templates with typed blanks
// that compile to lenses via /api/ask. Blanks autocomplete from the same symbol
// index the lens params use; answers render in the Result pane with a confidence
// badge. See GRASPABILITY.md.

var QUESTIONS = [];
var qValues = {}; // questionID -> {blankKey: value}

var INTENT_LABEL = {
 understand: "Understand — what is this",
 change: "Change — where do I add / modify",
 diagnose: "Diagnose — where to look",
 observe: "Observe — what happened / changed"
};
var INTENT_ORDER = ["understand", "change", "diagnose", "observe"];
var CONF_NOTE = {
 lexical: "text match — may include false positives",
 structural: "matched by code shape (no type resolution)",
 cpg: "precise (CPG / Joern)",
 exact: "exact (git / tool output)"
};

// askApplicable mirrors the lens applicability check against the detected language.
function askApplicable(q){
 var langs = q.langs || [];
 return langs.indexOf("any") >= 0 || langs.indexOf(STATE.lang) >= 0;
}

function renderQuestions(){
 var box = el("qlist"); if(!box) return;
 box.innerHTML = "";
 INTENT_ORDER.forEach(function(intent){
  var qs = QUESTIONS.filter(function(q){ return q.intent === intent && askApplicable(q); });
  if(!qs.length) return;
  var h = document.createElement("h2"); h.textContent = INTENT_LABEL[intent] || intent; box.appendChild(h);
  qs.forEach(function(q){ box.appendChild(renderQuestionCard(q)); });
 });
 if(!box.children.length){
  box.innerHTML = "<p class=hint>No questions apply to <b>"+esc(STATE.lang||"this")+"</b> sources yet. Switch source root, or use the Lens tab.</p>";
 }
}

// renderQuestionCard turns a question template into a card: the template text with
// inline blank inputs in place of {key}, an Ask button, and a confidence tag.
function renderQuestionCard(q){
 qValues[q.id] = qValues[q.id] || {};
 var card = document.createElement("div"); card.className = "qcard"; card.dataset.q = q.id;
 var line = document.createElement("div"); line.className = "qtmpl";
 var blanksByKey = {}; (q.blanks||[]).forEach(function(b){ blanksByKey[b.key] = b; });
 // Split the template on {key} tokens, interleaving text and blank inputs.
 var parts = q.template.split(/(\{[a-z_]+\})/i);
 parts.forEach(function(part){
  var m = part.match(/^\{([a-z_]+)\}$/i);
  if(m && blanksByKey[m[1]]){
   line.appendChild(makeBlank(q, blanksByKey[m[1]]));
  } else if(part){
   line.appendChild(document.createTextNode(part));
  }
 });
 card.appendChild(line);
 var foot = document.createElement("div"); foot.className = "qfoot";
 var ask = document.createElement("button"); ask.textContent = "Ask"; ask.className = "qask";
 ask.onclick = function(){ runQuestion(q); };
 foot.appendChild(ask);
 var tag = document.createElement("span"); tag.className = "conf conf-"+q.conf; tag.textContent = q.conf;
 tag.title = CONF_NOTE[q.conf] || q.conf;
 foot.appendChild(tag);
 card.appendChild(foot);
 return card;
}

// makeBlank builds one inline fill-in input with autocomplete (reusing combobox +
// fieldFetcher from core.js where the blank kind is a symbol kind).
function makeBlank(q, b){
 var wrap = document.createElement("span"); wrap.className = "field qblank";
 var inp = document.createElement("input"); inp.placeholder = b.hint || b.key; inp.autocomplete = "off";
 inp.value = qValues[q.id][b.key] || "";
 inp.oninput = function(){ qValues[q.id][b.key] = inp.value; };
 inp.onkeydown = function(e){ if(e.key === "Enter"){ e.preventDefault(); runQuestion(q); } };
 wrap.appendChild(inp);
 var fetcher = fieldFetcher(b.kind, function(){ return qValues[q.id].file || ""; });
 if(fetcher){
  var ac = document.createElement("div"); ac.className = "ac"; wrap.appendChild(ac);
  combobox(inp, ac, fetcher, function(it){ inp.value = it.value; qValues[q.id][b.key] = it.value;
   // a method pick may also carry its file — prefill a sibling file blank if empty
   if(it.file && (q.blanks||[]).some(function(x){return x.key==="file"}) && !qValues[q.id].file){
    qValues[q.id].file = it.file;
    var fb = wrap.parentElement.querySelector(".qblank input"); // best-effort; user can adjust
    void fb;
   }
  });
 }
 return wrap;
}

function runQuestion(q){
 showLensView(); tab("result");
 el("answerbadge").style.display = "none";
 el("out").className = ""; el("out").textContent = "asking…"; el("extra").innerHTML = "";
 fetch("/api/ask", {method:"POST", headers:{"Content-Type":"application/json"},
  body: JSON.stringify({id:q.id, source_root:STATE.sourceRoot, values:qValues[q.id]||{}})})
  .then(function(r){return r.json()}).then(function(d){
   if(d.error){
    el("out").className = "err";
    el("out").textContent = "couldn't answer: " + d.error +
     ((q.langs && q.langs.indexOf("any")<0 && q.langs.indexOf(STATE.lang)<0)
      ? "\n\n(this question is "+q.langs.join("/")+"-only; detected language is "+STATE.lang+")" : "");
    return;
   }
   var badge = el("answerbadge"); badge.style.display = "";
   badge.className = "answerbadge conf-"+(d.confidence||"lexical");
   badge.innerHTML = "<b>"+esc(q.template.replace(/\{[a-z_]+\}/gi, "…"))+"</b> · <span class=conf conf-"+
    esc(d.confidence||"lexical")+">"+esc(d.confidence||"")+"</span> "+esc(d.conf_note||CONF_NOTE[d.confidence]||"")+
    " · via <code>"+esc(d.analyzer||"")+"</code>";
   el("out").className = "";
   el("out").textContent = d.body || "(no results)";
   if(d.extra && d.extra.length){ el("extra").innerHTML = d.extra.map(function(e){return "<h2>"+esc(e.path)+"</h2><pre>"+esc(e.body)+"</pre>"}).join(""); }
   if(typeof saveLast === "function") saveLast();
  }).catch(function(e){ el("out").className="err"; el("out").textContent = String(e); });
}

// ---- left-panel mode toggle (Ask vs Lens) ------------------------------------
function setLeftMode(mode){
 var ask = mode === "ask";
 el("mAsk").classList.toggle("on", ask); el("mLens").classList.toggle("on", !ask);
 el("askpane").style.display = ask ? "" : "none";
 el("lenspane").style.display = ask ? "none" : "";
}
el("mAsk").onclick = function(){ setLeftMode("ask"); };
el("mLens").onclick = function(){ setLeftMode("lens"); };
