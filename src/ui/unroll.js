// unroll.js — the "Fix" (unrolled-program) tab: discover → choose → edit, with
// per-line assumptions, object timeline, loop bands and scattered two-way sync.
var uFile="",uMethod="";
function setEntryDisplay(){el("uentry").value=(uFile&&uMethod)?(uFile+" › "+uMethod):(uFile||uMethod||"")}
function prefillEntry(){uFile=STATE.defaults.entry_file||"";uMethod=STATE.defaults.entry_method||"";setEntryDisplay()}
combobox(el("uentry"),el("entryac"),
 function(q,cb){
  var term=q.split("›").pop().trim();
  symFetch(function(s){return s.kind==="method"||s.kind==="func"},term,function(a){
   cb(a.map(function(s){return{label:s.name,sub:s.file+":"+s.line,value:s.name,file:s.file}}))})},
 function(it){uFile=it.file;uMethod=it.value;setEntryDisplay();discover()});

var branchState={},inlineSkipState={},originalLines={},dirtyLines={};
function branchString(){var out=[];Object.keys(branchState).sort().forEach(function(k){if(branchState[k])out.push(k+"="+branchState[k])});return out.join(",")}
function inlineSkipString(){return Object.keys(inlineSkipState).filter(function(k){return inlineSkipState[k]}).sort().join(",")}
function inlineDepth(){return parseInt(el("uinline").value||"0",10)}
function updateInlineLabel(){el("uinlinelabel").textContent=String(inlineDepth())}
function ufields(){return {SourceRoot:STATE.sourceRoot,File:uFile,Method:uMethod,Inputs:el("uinputs").value,Branches:branchString(),InlineDepth:String(inlineDepth()),InlineSkips:inlineSkipString()}}
function sideLabel(side){return side==="then"?"true":"false"}
function callDepthLabel(c){return "level "+(c.depth+1)+" · "+(c.origin||c.id).split("/").pop()}
function renderRail(d){
 var choices=d.choices||[],calls=d.calls||[],bar=el("branchbar"),body=el("unrollbody");bar.innerHTML="";
 body.classList.toggle("no-side",!choices.length&&!calls.length);
 if(!choices.length&&!calls.length)return;
 if(calls.length){
  bar.innerHTML+="<span class=title>calls</span>";
  calls.forEach(function(c){
   var t=document.createElement("div");t.className="btab";
   var label=document.createElement("span");label.className="where";label.textContent=c.name||c.id;t.appendChild(label);
   var meta=document.createElement("span");meta.className="meta";meta.textContent=callDepthLabel(c);t.appendChild(meta);
   var sw=document.createElement("span");sw.className="bswitch";
   ["source","inline"].forEach(function(mode){
    var b=document.createElement("button");b.textContent=mode;b.className=((mode==="inline")?c.expanded:!c.expanded)?"on":"";
    b.onclick=function(){
     if(mode==="inline"){delete inlineSkipState[c.id];if(inlineDepth()<=c.depth){el("uinline").value=String(Math.min(10,c.depth+1));updateInlineLabel()}}
     else inlineSkipState[c.id]=true;
     discover();};
    sw.appendChild(b);});
   t.appendChild(sw);bar.appendChild(t);});
 }
 if(choices.length){var ti=document.createElement("span");ti.className="title";ti.textContent="assumptions";bar.appendChild(ti)}
 choices.forEach(function(c,idx){
  if(!branchState[c.id])branchState[c.id]=c.side;
  var t=document.createElement("div");t.className="btab";
  var label=document.createElement("span");label.className="where";label.textContent=c.cond||("branch "+(idx+1));t.appendChild(label);
  var sw=document.createElement("span");sw.className="bswitch";
  (c.sides||[]).forEach(function(side){
   var b=document.createElement("button");b.textContent=sideLabel(side);b.className=branchState[c.id]===side?"on":"";
   b.onclick=function(){branchState[c.id]=side;discover()};sw.appendChild(b);});
  t.appendChild(sw);bar.appendChild(t);});
}
function resetDirty(){dirtyLines={};el("usync").disabled=true;el("usync").textContent="Sync changes"}
function markDirty(row,line,code){
 if(code===originalLines[line])delete dirtyLines[line];else dirtyLines[line]=code;
 row.classList.toggle("dirty",!!dirtyLines[line]);
 var n=Object.keys(dirtyLines).length;el("usync").disabled=n===0;el("usync").textContent=n?"Sync "+n+" change"+(n===1?"":"s"):"Sync changes";
}
function showAssume(e,anchor,guards){
 e.stopPropagation();
 var old=document.getElementById("assume");if(old)old.remove();
 var d=document.createElement("div");d.className="assume";d.id="assume";
 d.innerHTML="<div class=ah>must be true to reach this line</div>"+
  guards.map(function(g){return "<code>"+esc(g)+"</code>"}).join("<span class=amp>AND</span>");
 document.body.appendChild(d);
 var r=anchor.getBoundingClientRect();
 d.style.left=(r.right+8)+"px";d.style.top=(window.scrollY+r.top)+"px";
 setTimeout(function(){document.addEventListener("click",function h(){d.remove();document.removeEventListener("click",h)})},0);
}
var EXIT_RE=/^\s*(return|break|continue|throw)\b/;
var IF_RE=/^\s*(if|else if)\b/;
// Tag early-returns/guards/dead-tail and try/catch structure on the rendered rows.
function tagFlow(items){
 // indentation + trimmed text per row
 items.forEach(function(it){var raw=it.code;it.t=raw.replace(/^\s+/,"");it.indent=raw.length-it.t.length});
 // 1) early return / break / continue / throw, and the conditional guarding them
 items.forEach(function(it,i){
  if(EXIT_RE.test(it.code)){it.row.classList.add("exit");
   // a preceding if on its own line whose body is this exit = a guard
   var p=items[i-1];if(p&&IF_RE.test(p.code))p.row.classList.add("guard");}
  if(IF_RE.test(it.code)&&EXIT_RE.test(it.code))it.row.classList.add("guard"); // inline: if(x) return;
 });
 // 2) try / catch / finally — headers + brace-only lines = struct; catch bodies fold
 items.forEach(function(it){
  if(/^try\b/.test(it.t)||/^\}?\s*finally\b/.test(it.t)){it.row.classList.add("struct")}
  if(/^\}?\s*catch\b/.test(it.t)){it.row.classList.add("struct");it.row.classList.add("catchhdr")}
  if(/^[{}]+;?$/.test(it.t)){it.row.classList.add("struct")}
 });
 // catch body lines (indented deeper than the catch header) — by indentation
 items.forEach(function(it,i){
  if(!it.row.classList.contains("catchhdr"))return;
  var bodyRows=[];
  for(var j=i+1;j<items.length;j++){if(items[j].indent<=it.indent)break;items[j].row.classList.add("catchbody");bodyRows.push(items[j].row)}
  it.row.querySelector(".code").onclick=function(){bodyRows.forEach(function(r){r.classList.toggle("show")})};
 });
 // 3) object mutations — write-count badges + per-var timeline
 var writeMap={};
 items.forEach(function(it,i){
  var w=lineWrites(it.t);
  if(w.length){it.writes=w;w.forEach(function(v){(writeMap[v]=writeMap[v]||[]).push(i)})}
 });
 items.forEach(function(it,i){
  if(!it.writes||it.row.classList.contains("struct"))return;
  var v=it.writes[0],list=writeMap[v],k=list.indexOf(i)+1;
  var chip=document.createElement("span");chip.className="wbadge";
  chip.textContent="✎ "+v+(list.length>1?" "+k+"/"+list.length:"");
  chip.title="write "+k+" of "+list.length+" to "+v+" — click for timeline";
  chip.onclick=function(e){showTimeline(e,chip,v,list,items)};
  it.row.appendChild(chip);
 });
 // 4) loop bands by indentation + effect summary at the head
 items.forEach(function(it,i){
  if(!/^(for|while|do)\b/.test(it.t))return;
  it.row.classList.add("loophead");
  var eff={};
  for(var j=i+1;j<items.length;j++){
   if(items[j].indent<=it.indent)break;
   items[j].row.classList.add("inloop");
   (items[j].writes||[]).forEach(function(v){eff[v]=(eff[v]||0)+1});
  }
  var keys=Object.keys(eff);
  if(keys.length){var s=document.createElement("span");s.className="loopeff";
   s.textContent="mutates: "+keys.map(function(v){return v+(eff[v]>1?"×"+eff[v]:"")}).join(", ");
   it.row.appendChild(s);}
 });
}
// lineWrites: which variable(s) a line writes (assign / compound / ++ / -- / setter).
function lineWrites(t){
 var m;
 if((m=t.match(/^([A-Za-z_]\w*)\s*(?:\+\+|--)\s*;?\s*$/))||(m=t.match(/^(?:\+\+|--)\s*([A-Za-z_]\w*)/)))return [m[1]];
 if((m=t.match(/\b([A-Za-z_]\w*)\.(set[A-Z]\w*)\s*\(/)))return [m[1]];
 if((m=t.match(/^(?:final\s+)?(?:[A-Za-z_][\w<>\[\].]*\s+)?([A-Za-z_]\w*)\s*(?:[-+*\/|&^]?=)[^=]/))){
  if(!/^(if|for|while|return|else|switch|case|do)$/.test(m[1]))return [m[1]];
 }
 return [];
}
function showTimeline(e,anchor,v,list,items){
 e.stopPropagation();
 var old=document.getElementById("tl");if(old){old._rows.forEach(function(r){r.classList.remove("wlit")});old.remove()}
 var lit=list.map(function(idx){return items[idx].row});lit.forEach(function(r){r.classList.add("wlit")});
 var d=document.createElement("div");d.className="assume";d.id="tl";d.style.maxWidth="40rem";d._rows=lit;
 d.innerHTML="<div class=ah>writes to <code>"+esc(v)+"</code> on this path — "+list.length+(list.length>1?" (set "+list.length+"×)":"")+"</div>"+
  list.map(function(idx,k){var o=items[idx].row.querySelector(".org");return "<div class=tlrow><b>"+(k+1)+"</b><span class=org>"+esc(o?o.textContent:"")+"</span><code>"+esc(items[idx].t)+"</code></div>"}).join("");
 document.body.appendChild(d);
 var r=anchor.getBoundingClientRect();
 d.style.left=Math.max(8,r.left-380)+"px";d.style.top=(window.scrollY+r.top)+"px";
 setTimeout(function(){document.addEventListener("click",function h(){lit.forEach(function(r){r.classList.remove("wlit")});d.remove();document.removeEventListener("click",h)})},0);
}
function renderProg(d){
 renderRail(d);resetDirty();originalLines={};
 var prog=el("uprog");prog.innerHTML="";var items=[];
 (d.lines||[]).forEach(function(l){
  originalLines[l.n]=l.code;
  var row=document.createElement("div");row.className="pl";row.dataset.line=l.n;
  var org=document.createElement("div");org.className="org";org.textContent=l.origin||"";
  var guards=l.guards||[];
  if(guards.length){row.classList.add("hasg");org.title="assumptions to reach this line — click";
   org.onclick=function(e){showAssume(e,org,guards)};}
  else{org.title=l.origin||"";if(l.origin)org.onclick=function(){var f=l.origin.split(":")[0];uFile=f;setEntryDisplay()}}
  var code=document.createElement("div");code.className="code";code.contentEditable="true";code.spellcheck=false;code.textContent=l.code;
  code.oninput=function(){markDirty(row,l.n,code.textContent)};
  code.onkeydown=function(e){if(e.key==="Enter"){e.preventDefault();code.blur()}if(e.key==="Tab"){e.preventDefault();document.execCommand("insertText",false,"    ")}};
  row.appendChild(org);row.appendChild(code);prog.appendChild(row);
  items.push({row:row,code:l.code});
 });
 tagFlow(items);
 prog.classList.toggle("fold",foldHandlers);
 var b=el("ubanner");
 if(d.unresolved){b.style.display="";b.className="banner";
  b.innerHTML=d.inputs?"⚠ Some branches depend on <b>runtime</b> values — both paths shown. Flip assumptions on the right to read one.":"① Unresolved branches. Type <b>inputs</b> on the left to collapse to the path that runs, or flip assumptions on the right.";}
 else if((d.lines||[]).length){b.style.display="";b.className="banner ok";
  b.innerHTML="✓ Editable path"+(d.inputs?" for <code>"+esc(d.inputs)+"</code>":"")+". Edit code directly, then Sync. Assumptions stay on the right.";}
 else b.style.display="none";
}
function discover(){
 if(typeof saveLast==="function")saveLast();
 if(!uFile||!uMethod){el("ustatus").textContent="pick an entry file › method above";el("uprog").innerHTML="";return}
 showUnrollView();el("ustatus").textContent="running…";
 fetch("/api/unroll",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(ufields())})
  .then(function(r){return r.json()}).then(function(d){
   if(d.error){el("ustatus").textContent="error: "+d.error;return}
   renderProg(d);el("ustatus").textContent="";});
}
var discoverD=debounce(discover,400);
var foldHandlers=false;
el("foldtc").onclick=function(){foldHandlers=!foldHandlers;el("uprog").classList.toggle("fold",foldHandlers);el("foldtc").textContent=foldHandlers?"Show try/catch":"Hide try/catch"};
el("uinputs").addEventListener("input",discoverD);
el("uinputs").addEventListener("keydown",function(e){if(e.key==="Enter")discover()});
el("uentry").addEventListener("input",function(){var v=el("uentry").value;if(v.indexOf("›")>=0){var p=v.split("›");uFile=p[0].trim();uMethod=p[1].trim()}});
el("uinline").addEventListener("input",updateInlineLabel);
el("uinline").addEventListener("change",discoverD);
function saveEdits(edits){
 if(!edits.length)return;
 el("ustatus").textContent="syncing…";var body=ufields();body.Edits=edits;
 fetch("/api/unroll/edit",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(body)})
  .then(function(r){return r.json()}).then(function(d){
   if(d.error){el("ustatus").textContent="error: "+d.error;return}
   renderProg(d);el("uprog").querySelectorAll(".pl").forEach(function(r){r.classList.add("flash")});
   el("ustatus").innerHTML="✓ synced ("+d.synced+")";flash("synced to source");});
}
el("usync").onclick=function(){
 var edits=Object.keys(dirtyLines).map(function(k){return {Line:parseInt(k),NewCode:dirtyLines[k]}}).sort(function(a,b){return a.Line-b.Line});
 saveEdits(edits);
};
