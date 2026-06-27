// trace.js — cross-repo trace tab + the project picker (top bar + modal).
// Trace by SYMBOL: the active config project is searched automatically; optionally
// its libraries too. No file/line/repo entry. Project management writes config.json
// via /api/projects (the single source of truth). Plain DOM, no framework.

var PROJECTS = { projects: [], active: "" };
var PROJ_ADD_TARGET = "";

// ---- project picker (top bar) ------------------------------------------------
function loadProjects(view){
 var done = function(d){ PROJECTS = d || {projects:[],active:""}; renderProjSelect(); renderProjList(); if(window.refreshGraphScope)refreshGraphScope(); };
 if(view){ done(view); return; }
 fetch("/api/projects").then(function(r){return r.json()}).then(done).catch(function(){});
}

function renderProjSelect(){
 var sel = el("projsel"); if(!sel) return;
 var ps = PROJECTS.projects || [];
 if(!ps.length){ sel.innerHTML = "<option value=''>(no project)</option>"; el("srcchip").style.display=""; return; }
 el("srcchip").style.display = "none";
 sel.innerHTML = ps.map(function(p){
  var on = p.name === PROJECTS.active ? " selected" : "";
  return "<option value='"+esc(p.name)+"'"+on+">"+esc(p.name)+" ("+ (p.repos||[]).length +" repos)</option>";
 }).join("");
 sel.onchange = function(){ setActiveProject(sel.value); };
}

function setActiveProject(name){
 postProjects({action:"set-active",project:name}, function(){ if(typeof reDetect==="function")reDetect(); });
}

function activeProjectObj(){ return (PROJECTS.projects||[]).filter(function(p){return p.name===PROJECTS.active})[0] || (PROJECTS.projects||[])[0]; }

// ---- project modal -----------------------------------------------------------
function openProjModal(){ el("projmodal").style.display=""; PROJ_ADD_TARGET = PROJECTS.active || ""; el("projaddtarget").textContent = PROJ_ADD_TARGET || "(create a project first)"; renderProjList(); }
function closeProjModal(){ el("projmodal").style.display="none"; }

function renderProjList(){
 var box = el("projlist"); if(!box) return;
 var ps = PROJECTS.projects || [];
 if(!ps.length){ box.innerHTML = "<p class=hint>No projects yet. Create one below, then add your app repo and its libraries.</p>"; return; }
 box.innerHTML = "";
 ps.forEach(function(p){
  var card = document.createElement("div"); card.className = "projcard"+(p.name===PROJECTS.active?" on":"");
  var repos = (p.repos||[]).map(function(r){
   var dep = (r.internal_deps&&r.internal_deps.length)?(" <span class=wsdep>↳ "+r.internal_deps.map(esc).join(", ")+"</span>"):"";
   return "<div class=wsrepo><div class=wsrow><b>"+esc(r.name)+"</b> <span class=wskind>"+esc(r.role||"app")+"</span> <span class=wsgroup>"+esc(r.group||"no group")+"</span>"+dep+
     "<button class=ghost data-rmrepo='"+esc(r.name)+"' data-proj='"+esc(p.name)+"'>×</button></div><div class=wspath>"+esc(r.path)+"</div></div>";
  }).join("");
  card.innerHTML = "<div class=projhd><b>"+esc(p.name)+"</b>"+(p.name===PROJECTS.active?" <span class=activetag>active</span>":" <button class=ghost data-activate='"+esc(p.name)+"'>activate</button>")+
    "<button class=ghost data-addto='"+esc(p.name)+"'>+ repo</button><button class=ghost data-rmproj='"+esc(p.name)+"'>delete</button></div>"+repos;
  box.appendChild(card);
 });
 box.querySelectorAll("[data-activate]").forEach(function(b){b.onclick=function(){setActiveProject(b.dataset.activate)}});
 box.querySelectorAll("[data-addto]").forEach(function(b){b.onclick=function(){PROJ_ADD_TARGET=b.dataset.addto;el("projaddtarget").textContent=PROJ_ADD_TARGET}});
 box.querySelectorAll("[data-rmproj]").forEach(function(b){b.onclick=function(){if(confirm("Delete project "+b.dataset.rmproj+"?"))delProjects("project="+encodeURIComponent(b.dataset.rmproj))}});
 box.querySelectorAll("[data-rmrepo]").forEach(function(b){b.onclick=function(){delProjects("project="+encodeURIComponent(b.dataset.proj)+"&repo="+encodeURIComponent(b.dataset.rmrepo))}});
}

function postProjects(body, after){
 el("projmsg")&&(el("projmsg").textContent="saving…");
 fetch("/api/projects",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(body)})
  .then(function(r){return r.json()}).then(function(d){
   if(d.error){ (el("projmsg")||{}).textContent="error: "+d.error; flash(d.error,1); return; }
   el("projmsg")&&(el("projmsg").textContent="saved");
   loadProjects(d.projects); if(after)after();
  }).catch(function(e){ flash(String(e),1); });
}
function delProjects(qs){
 fetch("/api/projects?"+qs,{method:"DELETE"}).then(function(r){return r.json()}).then(function(d){ if(d.error){flash(d.error,1);return} loadProjects(d.projects); });
}

// ---- trace by symbol ---------------------------------------------------------
function traceSymFetch(q,cb){
 fetch("/api/trace-symbols?project="+encodeURIComponent(PROJECTS.active||"")+"&q="+encodeURIComponent(q||""))
  .then(function(r){return r.json()}).then(function(d){
   cb((d.symbols||[]).map(function(s){return {label:s.name, sub:s.kind+" · "+s.repo, value:s.name}}));
  }).catch(function(){cb([])});
}

function runTrace(){
 var sym = el("trsym").value.trim();
 if(!sym){ el("tracestatus").textContent="type a symbol to trace"; return; }
 el("tracestatus").textContent="tracing "+sym+" …"; el("traceout").innerHTML="";
 fetch("/api/trace",{method:"POST",headers:{"Content-Type":"application/json"},
  body:JSON.stringify({symbol:sym, include_libraries:el("trlibs").checked})})
  .then(function(r){return r.json()}).then(renderTrace)
  .catch(function(e){el("tracestatus").textContent="error: "+e});
}

function renderTrace(d){
 if(d.error){el("tracestatus").textContent="error: "+d.error;return}
 el("tracestatus").textContent="";
 var out=el("traceout"); out.innerHTML="";
 if(d.summary&&d.summary.length){ var s=document.createElement("pre"); s.className="tracesum"; s.textContent=d.summary.join("\n"); out.appendChild(s); }
 var answers=d.answers||[];
 if(!answers.length){
  out.appendChild(note("No entrypoint path found. The method may be an entrypoint itself, dead, or only reached via reflection/proxy."));
  if(!el("trlibs").checked && d.libraries && d.libraries.length){
   var hint=document.createElement("button"); hint.className="expandlib"; hint.textContent="↳ expand with "+d.libraries.join(", ");
   hint.onclick=function(){ el("trlibs").checked=true; runTrace(); }; out.appendChild(hint);
  }
  return;
 }
 answers.forEach(function(a,i){
  var card=document.createElement("div"); card.className="answer";
  card.innerHTML="<div class=answerhd>answer "+(i+1)+"<span class=anote>"+esc(a.note||"")+"</span></div>";
  var pre=document.createElement("pre"); pre.className="answerbody"; pre.innerHTML=(a.lines||[]).map(decorate).join("\n");
  card.appendChild(pre); out.appendChild(card);
 });
 if(!el("trlibs").checked && d.libraries && d.libraries.length){
  var b=document.createElement("button"); b.className="expandlib"; b.textContent="↳ more paths may exist via "+d.libraries.join(", ")+" — expand";
  b.onclick=function(){ el("trlibs").checked=true; runTrace(); }; out.appendChild(b);
 }
}

function decorate(l){
 var s=esc(l);
 if(l.indexOf("[entry]")>=0) return "<span class=tl-entry>"+s+"</span>";
 if(l.indexOf("DI:")>=0) return "<span class=tl-di>"+s+"</span>";
 if(l.indexOf("crosses repo boundary")>=0) return "<span class=tl-cross>"+s+"</span>";
 if(l.indexOf("assume:")>=0) return "<span class=tl-assume>"+s+"</span>";
 if(l.indexOf("loop:")>=0) return "<span class=tl-loop>"+s+"</span>";
 if(l.indexOf("\u2605")>=0) return "<span class=tl-target>"+s+"</span>";
 return s;
}
function note(t){var p=document.createElement("p");p.className="hint";p.textContent=t;return p}

// loadWorkspace kept as the trace tab's entry hook (graph.js calls it on tab open).
function loadWorkspace(){ loadProjects(); }
window.loadWorkspace = loadWorkspace;

// ---- wiring ------------------------------------------------------------------
el("projadd")&&(el("projadd").onclick=openProjModal);
el("projclose")&&(el("projclose").onclick=closeProjModal);
el("projnewbtn")&&(el("projnewbtn").onclick=function(){ var n=el("projname").value.trim(); if(!n)return; postProjects({action:"new-project",project:n},function(){el("projname").value=""}); });
el("repadd")&&(el("repadd").onclick=function(){
 if(!PROJ_ADD_TARGET){ flash("create or pick a project first",1); return; }
 postProjects({action:"add-repo",project:PROJ_ADD_TARGET,path:el("reppath").value.trim(),url:el("repurl").value.trim(),role:el("reprole").value},
  function(){ el("reppath").value="";el("repurl").value=""; el("repbrowser").style.display="none"; });
});
el("trrun")&&(el("trrun").onclick=runTrace);
el("trsym")&&el("trsym").addEventListener("keydown",function(e){if(e.key==="Enter")runTrace()});
if(el("trsym")){ var ac=el("trsymac"); combobox(el("trsym"),ac,traceSymFetch,function(it){el("trsym").value=it.value}); }

// ---- folder chooser for "Add a repo" (reuses /api/dirs, absolute paths allowed) --
var REPBROWSE_PATH = "";
function repBrowse(path){
 fetch("/api/dirs?path="+encodeURIComponent(path||"")).then(function(r){return r.json()}).then(function(d){
  if(d.error){ el("repbrowser").innerHTML="<div class=note>"+esc(d.error)+"</div>"; return; }
  REPBROWSE_PATH = d.path||"";
  var h="<div class=crumb><span>"+esc(REPBROWSE_PATH||"(root)")+"</span><button class=ghost type=button id=repuse>Use this folder</button></div>";
  var parent = REPBROWSE_PATH.indexOf("/")>=0 ? REPBROWSE_PATH.slice(0,REPBROWSE_PATH.lastIndexOf("/")) : "";
  if(REPBROWSE_PATH) h+="<div class=di data-d='"+esc(parent)+"'>↑ ..</div>";
  (d.dirs||[]).forEach(function(dir){var full=REPBROWSE_PATH?(REPBROWSE_PATH+"/"+dir):dir;h+="<div class=di data-d='"+esc(full)+"'>📁 "+esc(dir)+"</div>"});
  var box=el("repbrowser"); box.innerHTML=h; box.style.display="";
  box.querySelector("#repuse").onclick=function(){ el("reppath").value=REPBROWSE_PATH; box.style.display="none"; };
  box.querySelectorAll(".di").forEach(function(n){n.onclick=function(){repBrowse(n.dataset.d)}});
 });
}
el("repbrowse")&&(el("repbrowse").onclick=function(){
 var box=el("repbrowser");
 if(box.style.display!=="none"&&box.innerHTML){ box.style.display="none"; return; }
 repBrowse(el("reppath").value.trim()||"");
});
