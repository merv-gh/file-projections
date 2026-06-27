// lenswizard.js — the guided "Add a lens" flow. Loads templates from
// /api/lens-templates (examples prefilled for the active project), lets the user
// pick one, tweak the example params, and saves a lens to config.json via
// /api/lenses. SQL-watch is a first-class template with discovered table names.

var LENS_TMPLS = [];

function openLensWizard(){
 el("lensmodal").style.display="";
 el("lensform").style.display="none";
 el("lensmsg").textContent="loading…";
 fetch("/api/lens-templates").then(function(r){return r.json()}).then(function(d){
  LENS_TMPLS = d.templates||[];
  el("lensmsg").textContent="";
  renderLensTemplates(d.tables||[]);
 }).catch(function(e){ el("lensmsg").textContent="error: "+e; });
}
function closeLensWizard(){ el("lensmodal").style.display="none"; }

var INTENT_EMOJI = {understand:"◍",change:"✎",diagnose:"⚲",observe:"◷"};
function renderLensTemplates(tables){
 var box=el("lenstmpls"); box.innerHTML="";
 LENS_TMPLS.forEach(function(t){
  var card=document.createElement("div"); card.className="lenstmpl";
  card.innerHTML="<div class=ltitle>"+esc(INTENT_EMOJI[t.intent]||"")+" "+esc(t.title)+" <span class=lanlz>"+esc(t.analyzer)+"</span></div>"+
    "<div class=ldesc>"+esc(t.desc)+"</div>"+(t.note?"<div class=lnote>"+esc(t.note)+"</div>":"");
  card.onclick=function(){ openLensForm(t); };
  box.appendChild(card);
 });
}

function openLensForm(t){
 el("lenstmpls").style.display="none";
 var form=el("lensform"); form.style.display="";
 var rows = Object.keys(t.example||{}).map(function(k){
  var v=t.example[k];
  var big = v.length>40 || k==="connections";
  var input = big ? "<textarea id=lp_"+esc(k)+" rows=2>"+esc(v)+"</textarea>" : "<input id=lp_"+esc(k)+" value='"+esc(v)+"'>";
  return "<label>"+esc(k)+"</label>"+input;
 }).join("");
 form.innerHTML = "<div class=row style='justify-content:space-between'><h2 style=margin:0>"+esc(t.title)+"</h2><button class=ghost id=lensback>← back</button></div>"+
  "<p class=hint>"+esc(t.desc)+"</p>"+
  "<label>lens name</label><input id=lp_name value='"+esc(suggestLensName(t))+"'>"+
  rows+
  "<div class=row style='margin-top:.6rem'><button id=lenssave>Save lens</button><span class=note id=lenssavemsg></span></div>";
 el("lensback").onclick=function(){ form.style.display="none"; el("lenstmpls").style.display=""; };
 el("lenssave").onclick=function(){ saveLensFromForm(t); };
}

function suggestLensName(t){
 var p = (window.PROJECTS&&PROJECTS.active)||"project";
 return p+"-"+t.id;
}

function saveLensFromForm(t){
 var name = (el("lp_name")||{}).value || suggestLensName(t);
 var params={};
 Object.keys(t.example||{}).forEach(function(k){ var e=el("lp_"+k); if(e&&e.value.trim()!=="")params[k]=e.value.trim(); });
 el("lenssavemsg")&&(el("lenssavemsg").textContent="saving…");
 fetch("/api/lenses",{method:"POST",headers:{"Content-Type":"application/json"},
  body:JSON.stringify({name:name,analyzer:t.analyzer,source_root:STATE.sourceRoot||".",params:params})})
  .then(function(r){return r.json()}).then(function(d){
   if(d.error){ el("lenssavemsg").textContent="error: "+d.error; return; }
   el("lenssavemsg").textContent="saved "+name;
   flash("added lens "+name);
   // refresh config view + graph lens list if present
   if(typeof loadSavedLenses==="function")loadSavedLenses();
   setTimeout(closeLensWizard, 600);
  }).catch(function(e){ el("lenssavemsg").textContent="error: "+e; });
}

el("lensclose")&&(el("lensclose").onclick=closeLensWizard);
el("lensadd")&&(el("lensadd").onclick=openLensWizard);
