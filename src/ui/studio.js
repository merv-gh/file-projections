// studio.js — saved lenses (bookmark a lens + config), localStorage persistence, boot.

// ---- saved lenses (bookmark a lens + its config) -----------------------------
// Capture the current lens setup as a LensConfig-shaped params bag.
function currentLensParams(){
 var a=el("an").value;
 if(a==="unrolled-program")return {file:uFile,method:uMethod,inputs:el("uinputs").value,inline_depth:String(inlineDepth())};
 return collectParams();
}
function loadSavedLenses(sel){
 fetch("/api/lenses").then(function(r){return r.json()}).then(function(d){
  var s=el("savedlenses");s.innerHTML="<option value=''>★ saved lenses…</option>";
  (d.lenses||[]).forEach(function(l){var o=document.createElement("option");o.value=l.name;
   o.textContent=l.name+" — "+l.analyzer;o._lens=l;s.appendChild(o)});
  if(sel)s.value=sel;
 });
}
function applyLens(l){
 setSourceRoot(l.source_root||STATE.sourceRoot);
 reDetect(function(){
  var s=el("an");if(!Array.from(s.options).some(function(o){return o.value===l.analyzer})){var o=document.createElement("option");o.value=o.textContent=l.analyzer;s.appendChild(o)}
  s.value=l.analyzer;renderParams();
  var p=l.params||{};
  if(l.analyzer==="unrolled-program"){uFile=p.file||"";uMethod=p.method||"";setEntryDisplay();
   if(p.inputs!=null)el("uinputs").value=p.inputs;
   if(p.inline_depth!=null){el("uinline").value=p.inline_depth;updateInlineLabel()}
  }else{Object.keys(p).forEach(function(k){var i=el("p_"+k);if(i)i.value=p[k]})}
  autoPreview();
 });
}
el("savelens").onclick=function(){
 var def=(el("an").value==="unrolled-program"&&uMethod)?uMethod:el("an").value;
 var name=prompt("Save this lens as:",def);if(!name)return;
 fetch("/api/lenses",{method:"POST",headers:{"Content-Type":"application/json"},
  body:JSON.stringify({name:name,analyzer:el("an").value,source_root:STATE.sourceRoot,params:currentLensParams()})})
  .then(function(r){return r.json()}).then(function(d){if(d.error){flash(d.error,1);return}loadSavedLenses(name);flash("lens saved: "+name)});
};
el("dellens").onclick=function(){
 var name=el("savedlenses").value;if(!name){flash("pick a saved lens first",1);return}
 if(!confirm("Delete saved lens \""+name+"\"?"))return;
 fetch("/api/lenses",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({name:name,delete:true})})
  .then(function(r){return r.json()}).then(function(){loadSavedLenses();flash("deleted "+name)});
};
el("savedlenses").onchange=function(){var o=el("savedlenses").selectedOptions[0];if(o&&o._lens)applyLens(o._lens)};

// ---- persistence: remember last lens/entry per repo (localStorage) ------------
var LSKEY="fp:last:"+location.host+":"+location.pathname;
function saveLast(){
 try{localStorage.setItem(LSKEY,JSON.stringify({sourceRoot:STATE.sourceRoot,analyzer:el("an").value,
  file:uFile,method:uMethod,inputs:el("uinputs").value,inline:el("uinline").value}))}catch(e){}
}
function loadLast(){try{return JSON.parse(localStorage.getItem(LSKEY)||"null")}catch(e){return null}}

// ---- boot --------------------------------------------------------------------
fetch("/api/config").then(function(r){return r.json()}).then(function(d){
 el("cfgpath").textContent=d.path||"";el("cfg").value=JSON.stringify(d.config,null,2);
 STATE.analyzers=d.analyzers||[];STATE.applic=d.applicability||{};STATE.defaults=d.defaults||{};
 STATE.lang=STATE.defaults.language||"";
 loadSavedLenses();
 var last=loadLast();
 if(last&&last.sourceRoot){
  setSourceRoot(last.sourceRoot);
  reDetect(function(){
   var s=el("an");if(last.analyzer&&Array.from(s.options).some(function(o){return o.value===last.analyzer}))s.value=last.analyzer;
   renderParams();
   if(last.file){uFile=last.file;uMethod=last.method||"";setEntryDisplay()}
   if(last.inputs)el("uinputs").value=last.inputs;
   if(last.inline){el("uinline").value=last.inline;updateInlineLabel()}
   autoPreview();
  });
 }else{
  setSourceRoot(STATE.defaults.source_root||".");
  el("langtag").textContent=STATE.lang;
  buildAnalyzerSelect();renderParams();prefillEntry();autoPreview();
 }
});
updateInlineLabel();
