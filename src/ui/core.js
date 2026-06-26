// core.js — shared helpers, state, param panel, preview, analyzer select, source root, clone.
var msg=document.getElementById("msg");
function el(id){return document.getElementById(id)}
function flash(t,bad){msg.textContent=t;msg.className=bad?"err":"ok";setTimeout(function(){if(msg.textContent===t)msg.textContent=""},2500)}
function esc(s){return String(s||"").replace(/[&<>"]/g,function(c){return{"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;"}[c]})}
function debounce(fn,ms){var t;return function(){clearTimeout(t);var a=arguments,self=this;t=setTimeout(function(){fn.apply(self,a)},ms)}}

var STATE={lang:"",sourceRoot:".",analyzers:[],applic:{},defaults:{},specs:{}};
// SCHEMA/HINTS are derived from the server's analyzer specs (/api/config -> specs),
// the single source of truth — no hand-maintained mirror that can drift from Go.
function schemaFor(a){var s=STATE.specs[a];return (s&&s.params)||[]}
function hintFor(a){var s=STATE.specs[a];return (s&&s.hint)||""}

// ---- autosuggest combobox ----------------------------------------------------
function symFetch(kindFilter,q,cb){
 fetch("/api/symbols?root="+encodeURIComponent(STATE.sourceRoot)+"&q="+encodeURIComponent(q||""))
  .then(function(r){return r.json()}).then(function(d){cb((d.symbols||[]).filter(kindFilter))}).catch(function(){cb([])})}
function varFetch(file,q,cb){
 fetch("/api/vars?root="+encodeURIComponent(STATE.sourceRoot)+"&file="+encodeURIComponent(file||"")+"&q="+encodeURIComponent(q||""))
  .then(function(r){return r.json()}).then(function(d){cb(d.vars||[])}).catch(function(){cb([])})}
// attach a dropdown to an input. opts.fetch(q,cb) -> items[{label,sub,value,extra}]; pick(item)
function combobox(input,box,fetcher,pick){
 var run=debounce(function(){
  fetcher(input.value,function(items){
   items=items.slice(0,40);
   if(!items.length){box.style.display="none";box.innerHTML="";return}
   box.innerHTML=items.map(function(it,i){return "<div class=acitem data-i='"+i+"'><b>"+esc(it.label)+"</b>"+(it.sub?"<span>"+esc(it.sub)+"</span>":"")+"</div>"}).join("");
   box.style.display="block";
   box.querySelectorAll(".acitem").forEach(function(n){n.onclick=function(e){e.stopPropagation();pick(items[parseInt(n.dataset.i)]);box.style.display="none"}});
  });
 },120);
 input.addEventListener("input",run);input.addEventListener("focus",run);
}
document.addEventListener("click",function(e){if(!e.target.closest(".field"))document.querySelectorAll(".ac").forEach(function(b){b.style.display="none"})});

// ---- param panel -------------------------------------------------------------
function fieldFetcher(t,getFile){
 if(t==="file")return function(q,cb){symFetch(function(s){return s.kind==="file"},q,function(a){cb(a.map(function(s){return{label:s.file,sub:s.kind,value:s.file}}))})};
 if(t==="method")return function(q,cb){symFetch(function(s){return s.kind==="method"||s.kind==="func"},q,function(a){cb(a.map(function(s){return{label:s.name,sub:s.file+":"+s.line,value:s.name,file:s.file}}))})};
 if(t==="type")return function(q,cb){symFetch(function(s){return["class","interface","enum","record","type"].indexOf(s.kind)>=0},q,function(a){cb(a.map(function(s){return{label:s.name,sub:s.kind+" · "+s.file,value:s.name}}))})};
 if(t==="var")return function(q,cb){varFetch(getFile(),q,function(a){cb(a.map(function(v){return{label:v,value:v}}))})};
 return null;
}
function paramVal(k){var i=el("p_"+k);return i?i.value:""}
function getParamFile(){return paramVal("file")}
function renderParams(){
 var a=el("an").value,box=el("params");box.innerHTML="";el("anhint").textContent=hintFor(a);
 if(a==="unrolled-program"){el("unrollctl").style.display="";return}
 el("unrollctl").style.display="none";
 var schema=schemaFor(a),d=STATE.defaults;
 schema.forEach(function(f){
  var lab=document.createElement("label");lab.textContent=f.k+(f.required?" *":"");box.appendChild(lab);
  if(f.t==="select"){
   var s=document.createElement("select");s.id="p_"+f.k;(f.opts||[]).forEach(function(o){var op=document.createElement("option");op.value=op.textContent=o;s.appendChild(op)});box.appendChild(s);
   s.onchange=autoPreviewD;return;
  }
  var wrap=document.createElement("div");wrap.className="field";
  var inp=f.t==="text"?document.createElement("textarea"):document.createElement("input");inp.id="p_"+f.k;inp.autocomplete="off";
  if(f.t==="text")inp.rows=f.k==="connections"?4:2;
  // prefill from real-repo defaults
  if(f.k==="file")inp.value=d.entry_file||"";
  else if(f.k==="method")inp.value=d.entry_method||"";
  else if(f.k==="line")inp.value=d.entry_line||"";
  else if(f.k==="var")inp.value=d.example_var||"";
  else if(f.k==="type")inp.value=d.example_type||"";
  else if(f.ex)inp.placeholder=f.ex;
  if(f.ex)inp.placeholder=f.ex;
  wrap.appendChild(inp);
  var fetcher=fieldFetcher(f.t,getParamFile);
  if(fetcher){var ac=document.createElement("div");ac.className="ac";wrap.appendChild(ac);
   combobox(inp,ac,fetcher,function(it){inp.value=it.value;autoPreview()});}
  inp.addEventListener("input",autoPreviewD);
  box.appendChild(wrap);
 });
}
function collectParams(){var a=el("an").value,o={};schemaFor(a).forEach(function(f){var v=paramVal(f.k);if(v!=="")o[f.k]=v});return o}

// ---- preview -----------------------------------------------------------------
function showLensView(){el("lensview").style.display="";el("unrollview").style.display="none"}
function showUnrollView(){el("lensview").style.display="none";el("unrollview").style.display=""}
function autoPreview(){
 tab("result");
 if(typeof saveLast==="function")saveLast();
 var a=el("an").value;
 if(a==="unrolled-program"){showUnrollView();discover();return}
 showLensView();
 el("out").textContent="running "+a+"…";el("out").className="";el("extra").innerHTML="";
 fetch("/api/preview",{method:"POST",headers:{"Content-Type":"application/json"},
  body:JSON.stringify({analyzer:a,source_root:STATE.sourceRoot,params:collectParams()})})
 .then(function(r){return r.json()}).then(function(d){
  if(d.error){el("out").textContent="error: "+d.error;el("out").className="err";return}
  el("out").className="";
  el("out").textContent=(d.body||"(empty projection)")+"\n— "+d.blocks+" blocks · "+d.facts+" facts · "+d.sync;
  if(d.extra&&d.extra.length){el("extra").innerHTML=d.extra.map(function(e){return "<h2>"+esc(e.path)+"</h2><pre>"+esc(e.body)+"</pre>"}).join("")}
 }).catch(function(e){el("out").textContent=String(e);el("out").className="err"});
}
var autoPreviewD=debounce(autoPreview,350);

// ---- analyzer select ---------------------------------------------------------
function applicable(a){var langs=STATE.applic[a]||[];return langs.indexOf("any")>=0||langs.indexOf(STATE.lang)>=0}
function buildAnalyzerSelect(){
 var s=el("an");s.innerHTML="";
 STATE.analyzers.filter(applicable).forEach(function(a){var o=document.createElement("option");o.value=o.textContent=a;s.appendChild(o)});
 var want=STATE.defaults.analyzer;
 if(want&&Array.from(s.options).some(function(o){return o.value===want}))s.value=want;
}
el("an")&&(el("an").onchange=function(){renderParams();autoPreview()});

// ---- source root + dir picker ------------------------------------------------
function setSourceRoot(root){STATE.sourceRoot=root||".";el("srval").textContent=STATE.sourceRoot}
function reDetect(after){
 fetch("/api/detect?root="+encodeURIComponent(STATE.sourceRoot)).then(function(r){return r.json()}).then(function(d){
  STATE.lang=d.language||STATE.lang;STATE.defaults=d.defaults||STATE.defaults;
  el("langtag").textContent=STATE.lang;
  buildAnalyzerSelect();renderParams();prefillEntry();
  if(after)after();else autoPreview();
 });
}
function openPicker(){
 var old=el("picker");if(old){old.remove();return}
 var anchor=el("srchange").getBoundingClientRect();
 var p=document.createElement("div");p.className="picker";p.id="picker";
 p.style.left=anchor.left+"px";p.style.top=(anchor.bottom+6)+"px";
 document.body.appendChild(p);
 var cur=STATE.sourceRoot==="."?"":STATE.sourceRoot;
 function load(path){
  fetch("/api/dirs?path="+encodeURIComponent(path)).then(function(r){return r.json()}).then(function(d){
   var here=d.path||".";
   var h="<div class=crumb><span>"+esc(here||".")+"</span><button class=ghost id=pkuse>Use this</button></div>";
   if(here&&here!==".")h+="<div class=di data-d='"+esc(parentOf(here))+"'>↑ ..</div>";
   (d.dirs||[]).forEach(function(dir){var full=(here&&here!==".")?here+"/"+dir:dir;h+="<div class=di data-d='"+esc(full)+"'>📁 "+esc(dir)+"</div>"});
   p.innerHTML=h;
   p.querySelector("#pkuse").onclick=function(){setSourceRoot(here||".");p.remove();reDetect()};
   p.querySelectorAll(".di").forEach(function(n){n.onclick=function(){load(n.dataset.d)}});
  });
 }
 load(cur);
}
function parentOf(p){var i=p.lastIndexOf("/");return i<0?"":p.slice(0,i)}
el("srchange").onclick=openPicker;

// ---- clone a github repo (server-side shallow clone, then use as source root) -
el("clonebtn").onclick=function(){
 var url=el("cloneurl").value.trim();if(!url){flash("paste a git URL or owner/repo",1);return}
 el("clonebtn").disabled=true;flash("cloning "+url+"…");
 fetch("/api/clone",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({url:url})})
  .then(function(r){return r.json()}).then(function(d){
   el("clonebtn").disabled=false;
   if(d.error){flash(d.error,1);return}
   flash("cloned into "+d.root);
   setSourceRoot(d.root);reDetect();
  }).catch(function(e){el("clonebtn").disabled=false;flash(String(e),1)});
};
el("cloneurl").addEventListener("keydown",function(e){if(e.key==="Enter")el("clonebtn").click()});
