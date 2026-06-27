// trace.js — the cross-repo, dependency-inversion-aware trace-to-line panel.
// Registers the workspace repos (link a folder or clone), then asks for every
// control path from a (possibly library) entrypoint to a line. Talks to
// /api/workspace and /api/trace. Plain DOM, no framework (matches the other panels).

var WS=null;

function loadWorkspace(){
 fetch("/api/workspace").then(function(r){return r.json()}).then(function(d){
  WS=d; renderWorkspace(d);
 }).catch(function(e){el("tracestatus").textContent="workspace error: "+e});
}

function renderWorkspace(d){
 var box=el("wsrepos"); box.innerHTML="";
 var repos=(d&&d.repos)||[];
 var sel=el("trrepo"); sel.innerHTML="<option value=''>(any repo)</option>";
 if(!repos.length){ box.innerHTML="<p class=hint>No repos yet. Link your app repo and its internal libraries (or clone them) to trace across the boundary.</p>"; return; }
 repos.forEach(function(r){
  var card=document.createElement("div"); card.className="wsrepo";
  var grp=r.group?("<span class=wsgroup>"+esc(r.group)+"</span>"):"<span class=wsgroup style=color:var(--mut)>no gradle group</span>";
  var deps=(r.internal_deps&&r.internal_deps.length)?("<span class=wsdep>↳ internal: "+r.internal_deps.map(esc).join(", ")+"</span>"):"";
  card.innerHTML="<div class=wsrow><b>"+esc(r.name)+"</b> <span class=wskind>"+esc(r.kind)+"</span> "+grp+deps+
    "<button class=ghost data-rm='"+esc(r.name)+"' title='remove'>×</button></div>"+
    "<div class=wspath>"+esc(r.path)+"</div>";
  box.appendChild(card);
  var opt=document.createElement("option"); opt.value=r.name; opt.textContent=r.name; sel.appendChild(opt);
 });
 box.querySelectorAll("[data-rm]").forEach(function(b){
  b.onclick=function(){ if(!confirm("Remove "+b.dataset.rm+" from the workspace?"))return;
   fetch("/api/workspace?name="+encodeURIComponent(b.dataset.rm),{method:"DELETE"})
    .then(function(r){return r.json()}).then(function(d){ if(d.error){alert(d.error);return} renderWorkspace(d.workspace); }); };
 });
}

function addRepo(kind){
 var path=kind==="clone"?el("wsclone").value.trim():el("wslink").value.trim();
 if(!path)return;
 el("tracestatus").textContent=(kind==="clone"?"cloning ":"linking ")+path+" …";
 fetch("/api/workspace",{method:"POST",headers:{"Content-Type":"application/json"},
  body:JSON.stringify(kind==="clone"?{kind:"clone",url:path}:{kind:"link",path:path})})
  .then(function(r){return r.json()}).then(function(d){
   if(d.error){el("tracestatus").textContent="error: "+d.error;return}
   el("tracestatus").textContent="added "+(d.repo&&d.repo.name);
   el("wslink").value="";el("wsclone").value="";
   renderWorkspace(d.workspace);
  }).catch(function(e){el("tracestatus").textContent="error: "+e});
}

function runTrace(){
 var file=el("trfile").value.trim(), line=parseInt(el("trline").value,10);
 if(!file||!line){el("tracestatus").textContent="enter a file and line";return}
 el("tracestatus").textContent="tracing …"; el("traceout").innerHTML="";
 fetch("/api/trace",{method:"POST",headers:{"Content-Type":"application/json"},
  body:JSON.stringify({repo:el("trrepo").value,file:file,line:line})})
  .then(function(r){return r.json()}).then(function(d){ renderTrace(d) })
  .catch(function(e){el("tracestatus").textContent="error: "+e});
}

function renderTrace(d){
 if(d.error){el("tracestatus").textContent="error: "+d.error;return}
 el("tracestatus").textContent="";
 var out=el("traceout"); out.innerHTML="";
 if(d.summary&&d.summary.length){
  var s=document.createElement("pre"); s.className="tracesum"; s.textContent=d.summary.join("\n"); out.appendChild(s);
 }
 var answers=d.answers||[];
 if(!answers.length){ out.appendChild(note("No entrypoint path found. The method may be an entrypoint itself, dead, or only reached via reflection/proxy.")); return; }
 answers.forEach(function(a,i){
  var card=document.createElement("div"); card.className="answer";
  var badge="<span class=cbadge data-c='"+esc((a.confidence||"").replace(/[^a-z]/g,""))+"'>"+esc(a.confidence||"structural")+"</span>";
  card.innerHTML="<div class=answerhd>answer "+(i+1)+" "+badge+"<span class=anote>"+esc(a.note||"")+"</span></div>";
  var pre=document.createElement("pre"); pre.className="answerbody";
  pre.innerHTML=(a.lines||[]).map(decorate).join("\n");
  card.appendChild(pre); out.appendChild(card);
 });
}

// decorate adds light highlighting to the path lines: DI hops, repo crossings,
// assumptions, the target marker.
function decorate(l){
 var s=esc(l);
 if(l.indexOf("[entry]")>=0) return "<span class=tl-entry>"+s+"</span>";
 if(l.indexOf("DI:")>=0) return "<span class=tl-di>"+s+"</span>";
 if(l.indexOf("crosses repo boundary")>=0) return "<span class=tl-cross>"+s+"</span>";
 if(l.indexOf("assume:")>=0) return "<span class=tl-assume>"+s+"</span>";
 if(l.indexOf("loop:")>=0) return "<span class=tl-loop>"+s+"</span>";
 if(l.indexOf("★")>=0) return "<span class=tl-target>"+s+"</span>";
 return s;
}

function note(t){var p=document.createElement("p");p.className="hint";p.textContent=t;return p}
function esc(s){return String(s==null?"":s).replace(/[&<>]/g,function(c){return{"&":"&amp;","<":"&lt;",">":"&gt;"}[c]})}

el("wsaddlink").onclick=function(){addRepo("link")};
el("wsaddclone").onclick=function(){addRepo("clone")};
el("trrun").onclick=runTrace;
el("trline").addEventListener("keydown",function(e){if(e.key==="Enter")runTrace()});
window.loadWorkspace=loadWorkspace;
