// graph.js — result/graph/config tabs + the service-graph SVG renderer and mermaid export.
var GPANES={result:["tresult","resultpane"],graph:["tgraph","graphpane"],trace:["ttrace","tracepane"],cfg:["tcfg","cfgpane"]};
function tab(name){for(var k in GPANES){var on=k===name;el(GPANES[k][0]).classList.toggle("on",on);el(GPANES[k][1]).style.display=on?"":"none"}}
el("tresult").onclick=function(){tab("result")};el("tcfg").onclick=function(){tab("cfg")};
el("tgraph").onclick=function(){tab("graph");if(!GRAPH)loadGraph()};
el("ttrace").onclick=function(){tab("trace");if(window.loadWorkspace)loadWorkspace()};

var GRAPH=null;
var GRAPH_BASE="";
function loadGraph(lens){
 el("gstatus").textContent="building…";
 fetch("/api/graph"+(lens?"?lens="+encodeURIComponent(lens):"")).then(function(r){return r.json()}).then(function(d){
  var sel=el("graphlens");if(d.lenses){sel.innerHTML=d.lenses.map(function(n){return "<option"+(n===d.lens?" selected":"")+">"+esc(n)+"</option>"}).join("")}
  if(d.error){el("gstatus").textContent=d.error;el("graphwrap").innerHTML="<div class=note style=padding:1rem>"+esc(d.error)+" — add one to config.json (see Service graph in README).</div>";return}
  GRAPH=d.graph;GRAPH_BASE=(d.source_root&&d.source_root!==".")?d.source_root:"";el("gstatus").textContent="";renderGraph(GRAPH);
 }).catch(function(e){el("gstatus").textContent=String(e)});
}
el("graphlens").onchange=function(){GRAPH=null;loadGraph(el("graphlens").value)};
el("gcross").onchange=function(){if(GRAPH)renderGraph(GRAPH)};
el("geffonly").onchange=function(){if(GRAPH)renderGraph(GRAPH)};
el("gsearch")&&el("gsearch").addEventListener("input",debounce(function(){if(GRAPH&&el("graphwrap").style.display!=="none")renderGraph(GRAPH)},150));
// graph / tables sub-view toggle within the Service-graph tab.
function graphSubview(which){
 var tables=which==="tables";
 el("gvgraph").classList.toggle("on",!tables);el("gvtables").classList.toggle("on",tables);
 el("graphwrap").style.display=tables?"none":"";
 el("tableswrap").style.display=tables?"":"none";
 el("gsearch").placeholder=tables?"filter tables…":"filter nodes…";
 if(tables)loadTables();else if(GRAPH)renderGraph(GRAPH);
}
el("gvgraph")&&(el("gvgraph").onclick=function(){graphSubview("graph")});
el("gvtables")&&(el("gvtables").onclick=function(){graphSubview("tables")});
// legend chips toggle individual effect kinds on/off
Array.prototype.forEach.call(document.querySelectorAll("#efflegend .effk"),function(chip){
 chip.onclick=function(){
  var k=chip.dataset.k;EFF_OFF[k]=!EFF_OFF[k];chip.classList.toggle("off",!!EFF_OFF[k]);
  if(GRAPH)renderGraph(GRAPH);
 };
});
var EFF_OFF={}; // effect kinds toggled off via the legend
function graphNodesEdges(g){
 var crossOnly=el("gcross").checked;
 var effOnly=el("geffonly")&&el("geffonly").checked;
 var q=(el("gsearch")&&el("gsearch").value||"").trim().toLowerCase();
 var edges=g.edges,nodes=g.nodes;
 if(crossOnly){
  edges=edges.filter(function(e){return e.cross});
  var keep={};edges.forEach(function(e){keep[e.from]=1;keep[e.to]=1});
  nodes=nodes.filter(function(n){return keep[n.id]});
 }
 // search filter: keep matching nodes + their direct neighbors so an edge still has
 // both endpoints (so you see who a matched node connects to).
 if(q){
  var hit={};
  nodes.forEach(function(n){if((n.label||"").toLowerCase().indexOf(q)>=0||(n.id||"").toLowerCase().indexOf(q)>=0)hit[n.id]=1});
  var near={};
  edges.forEach(function(e){if(hit[e.from]||hit[e.to]){near[e.from]=1;near[e.to]=1}});
  Object.keys(hit).forEach(function(k){near[k]=1});
  nodes=nodes.filter(function(n){return near[n.id]});
 }
 // effect filters: hide nodes whose effects are all toggled off; "side-effects only"
 // restricts to nodes that perform at least one (still-enabled) effect.
 function visibleEffects(n){return (n.effects||[]).filter(function(k){return !EFF_OFF[k]})}
 if(effOnly){nodes=nodes.filter(function(n){return visibleEffects(n).length>0})}
 var ids={};nodes.forEach(function(n){ids[n.id]=1});
 edges=edges.filter(function(e){return ids[e.from]&&ids[e.to]});
 return {nodes:nodes,edges:edges};
}
function renderGraph(g){
 var ne=graphNodesEdges(g),nodes=ne.nodes,edges=ne.edges;
 var services=g.services.map(function(s){return s.name});
 var COLW=250,ROWH=26,PADTOP=34,PADX=14,NODEW=210,NODEH=20;
 var byCol={};services.forEach(function(s,i){byCol[s]=[]});
 nodes.forEach(function(n){if(byCol[n.service])byCol[n.service].push(n)});
 var pos={},maxRows=0;
 services.forEach(function(s,ci){
  byCol[s].forEach(function(n,ri){pos[n.id]={x:PADX+ci*COLW,y:PADTOP+ri*ROWH,col:ci,row:ri}});
  maxRows=Math.max(maxRows,byCol[s].length);
 });
 var W=PADX*2+services.length*COLW,H=PADTOP+maxRows*ROWH+20;
 var anchorR=function(id){var p=pos[id];return p?{x:p.x+NODEW,y:p.y+NODEH/2}:null};
 var anchorL=function(id){var p=pos[id];return p?{x:p.x,y:p.y+NODEH/2}:null};
 var svg=['<svg viewBox="0 0 '+W+' '+H+'" width="'+W+'" height="'+H+'" xmlns="http://www.w3.org/2000/svg">'];
 // column backgrounds + headers
 services.forEach(function(s,ci){
  var x=PADX+ci*COLW-6;
  svg.push('<rect class="gcolbg" x="'+x+'" y="6" width="'+(NODEW+12)+'" height="'+(H-12)+'" rx="8"></rect>');
  svg.push('<text class=glabel x="'+(x+8)+'" y="22">'+esc(s)+' ('+byCol[s].length+')</text>');
 });
 // edges (under nodes)
 edges.forEach(function(e){
  var a=anchorR(e.from),b=anchorL(e.to);if(!a||!b)return;
  // same column: route to the right and back
  var cls="gedge "+(e.kind==="api-call"?"apicall":e.kind)+(e.cross?" cross":"");
  var mx=(a.x+b.x)/2;
  var d;
  if(pos[e.from].col===pos[e.to].col){var off=24;d="M"+a.x+","+a.y+" C"+(a.x+off)+","+a.y+" "+(a.x+off)+","+b.y+" "+b.x+","+b.y;
   if(e.to< e.from){}}
  else d="M"+a.x+","+a.y+" C"+mx+","+a.y+" "+mx+","+b.y+" "+b.x+","+b.y;
  svg.push('<path class="'+cls+'" data-from="'+esc(e.from)+'" data-to="'+esc(e.to)+'" d="'+d+'"/>');
  if(e.label&&e.kind==="api-call"){svg.push('<text class=gedgelabel x="'+(mx)+'" y="'+((a.y+b.y)/2-2)+'" text-anchor=middle>'+esc(e.label)+'</text>')}
 });
 // nodes
 var EFFCOLOR={"db":"#b07a2b","network":"#3f6f9f","io-write":"#b54848","io-read":"#7a8a3a","process":"#7a4fa0"};
 nodes.forEach(function(n){
  var p=pos[n.id];if(!p)return;
  var effs=(n.effects||[]).filter(function(k){return !EFF_OFF[k]});
  var maxLabel=effs.length?24:30;
  var label=n.label.length>maxLabel?"…"+n.label.slice(-(maxLabel-1)):n.label;
  var dots="";
  effs.forEach(function(k,i){
   dots+='<circle class=geff cx="'+(NODEW-9-i*11)+'" cy="'+(NODEH/2)+'" r="4" fill="'+(EFFCOLOR[k]||"#888")+'"><title>'+esc(k)+'</title></circle>';
  });
  svg.push('<g class="gnode k'+n.kind+(effs.length?" haseff":"")+'" data-id="'+esc(n.id)+'" data-eff="'+esc(effs.join(","))+'" transform="translate('+p.x+','+p.y+')">'+
   '<rect width="'+NODEW+'" height="'+NODEH+'" rx="5"></rect>'+
   '<text x="7" y="14">'+esc(label)+'</text>'+dots+'</g>');
 });
 svg.push('</svg>');
 el("graphwrap").innerHTML=svg.join("");
 el("graphwrap").querySelectorAll(".gnode").forEach(function(gn){
  gn.onclick=function(){onGraphNode(g,gn.dataset.id)};
  gn.onmouseenter=function(){highlightNode(gn.dataset.id,true)};
  gn.onmouseleave=function(){highlightNode(gn.dataset.id,false)};
 });
}
function highlightNode(id,on){
 var wrap=el("graphwrap");
 if(!on){wrap.querySelectorAll(".dim").forEach(function(x){x.classList.remove("dim")});return}
 var keep={};keep[id]=1;
 wrap.querySelectorAll(".gedge").forEach(function(p){
  if(p.dataset.from===id||p.dataset.to===id){keep[p.dataset.from]=1;keep[p.dataset.to]=1}else p.classList.add("dim");
 });
 wrap.querySelectorAll(".gnode").forEach(function(gn){if(!keep[gn.dataset.id])gn.classList.add("dim")});
}
function onGraphNode(g,id){
 var n=g.nodes.filter(function(x){return x.id===id})[0];if(!n)return;
 if((n.lang==="go"||n.lang==="js")&&n.method){
  // drill into the handler: assumptions + object timeline (Go or TS)
  setSourceRoot(graphSourceRoot(g,n));
  reDetect(function(){
   var s=el("an");if(!Array.from(s.options).some(function(o){return o.value==="unrolled-program"})){var o=document.createElement("option");o.value=o.textContent="unrolled-program";s.appendChild(o)}
   s.value="unrolled-program";renderParams();
   uFile=goRelToService(g,n);uMethod=n.method;setEntryDisplay();tab("result");discover();
  });
  flash("drilling into "+n.method+"()");
 }else{
  flash(n.file+(n.op?(" · ops: "+n.op):""));
 }
}
// the graph source_root is the lens source_root (apps dir); for a drill we need the service root.
function graphSourceRoot(g,n){var svc=g.services.filter(function(s){return s.name===n.service})[0];return (GRAPH_BASE?GRAPH_BASE+"/":"")+(svc?svc.root:"")}
function goRelToService(g,n){var svc=g.services.filter(function(s){return s.name===n.service})[0];if(svc&&n.file.indexOf(svc.root+"/")===0)return n.file.slice(svc.root.length+1);return n.file}
function mermaid(g){
 var ne=graphNodesEdges(g),nodes=ne.nodes,edges=ne.edges;
 var id=function(s){return s.replace(/[^A-Za-z0-9]/g,"_")};
 var out=["graph LR"];
 g.services.forEach(function(s){
  var ns=nodes.filter(function(n){return n.service===s.name});if(!ns.length)return;
  out.push("  subgraph "+id(s.name)+"["+s.name+"]");
  ns.forEach(function(n){var shape=n.kind==="entrypoint"?["([","])"]:["[","]"];out.push("    "+id(n.id)+shape[0]+'"'+n.label.replace(/"/g,"")+'"'+shape[1])});
  out.push("  end");
 });
 edges.forEach(function(e){
  var arrow=e.kind==="api-call"?'-. "'+e.label+'" .->':(e.kind==="registers"?'-- '+e.label+' -->':"-->");
  out.push("  "+id(e.from)+" "+arrow+" "+id(e.to));
 });
 return out.join("\n");
}
el("gmermaid").onclick=function(){
 if(!GRAPH)return;var m=mermaid(GRAPH);el("mermaidtext").value=m;el("mermaidbox").style.display="";
 if(navigator.clipboard)navigator.clipboard.writeText(m).then(function(){flash("mermaid copied")},function(){});
};
el("gmclose").onclick=function(){el("mermaidbox").style.display="none"};
el("savecfg").onclick=function(){
 var body;try{body=JSON.parse(el("cfg").value)}catch(e){el("cfgmsg").textContent="invalid JSON: "+e;el("cfgmsg").className="note err";return}
 fetch("/api/config",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(body,null,2)})
 .then(function(r){return r.json()}).then(function(d){
  if(d.error){el("cfgmsg").textContent=d.error;el("cfgmsg").className="note err"}
  else{el("cfgmsg").textContent="saved · "+d.lenses+" lenses";el("cfgmsg").className="note ok";flash("config saved")}});
};

// ---- Tables view: find a table, see who writes/reads it, one-click trace ---------
var TABLES=null;
function loadTables(){
 el("gstatus").textContent="scanning tables…";
 fetch("/api/tables").then(function(r){return r.json()}).then(function(d){
  TABLES=d.tables||[];el("gstatus").textContent="";renderTables();
 }).catch(function(e){el("gstatus").textContent=String(e)});
}
function renderTables(){
 var box=el("tableswrap");box.innerHTML="";
 var q=(el("gsearch").value||"").trim().toLowerCase();
 var tables=(TABLES||[]).filter(function(t){return !q||t.name.toLowerCase().indexOf(q)>=0});
 if(!tables.length){box.innerHTML="<div class=note style=padding:1rem>No tables discovered. Add an app repo with JPA entities or SQL migrations to the project.</div>";return}
 tables.forEach(function(t){
  var card=document.createElement("div");card.className="tablecard";
  var writers=t.writers||[],readers=t.readers||[];
  var warn=writers.length>1?'<span class=tbug title="more than one place writes this table">⚠ '+writers.length+' writers</span>':'';
  var mig=(t.migrations&&t.migrations.length)?'<span class=tmig>'+esc(t.migrations[0])+(t.mig_repo?(" · "+esc(t.mig_repo)):"")+'</span>':'';
  function sites(list,kind){
   if(!list.length)return '<div class=tnone>no '+kind+'</div>';
   return list.map(function(s){
    return '<div class=tsite><span class=tsmethod>'+esc(s.method)+'</span> <span class=tsrepo>'+esc(s.repo)+'</span>'+
     '<span class=tsloc>'+esc(s.file.split("/").pop())+':'+s.line+'</span>'+
     '<button class=ghost data-trace="'+esc(t.name)+'" data-m="'+esc(s.method)+'">trace</button>'+
     '<div class=tscode>'+esc(s.code)+'</div></div>';
   }).join("");
  }
  card.innerHTML='<div class=thead><b class=tname>▱ '+esc(t.name)+'</b>'+
   (t.entity?'<span class=tentity>entity '+esc(t.entity)+'</span>':'')+mig+warn+
   '<button class=ghost data-tracetable="'+esc(t.name)+'">trace table</button></div>'+
   '<div class=tcols><div class=tcol><div class="tcolhd tcw">✎ writes here ('+writers.length+')</div>'+sites(writers,"writers")+'</div>'+
   '<div class=tcol><div class="tcolhd tcr">↻ reads here ('+readers.length+')</div>'+sites(readers,"readers")+'</div></div>';
  box.appendChild(card);
 });
 box.querySelectorAll("[data-tracetable]").forEach(function(b){b.onclick=function(){traceTable(b.dataset.tracetable)}});
 box.querySelectorAll("[data-trace]").forEach(function(b){b.onclick=function(){traceTable(b.dataset.trace)}});
}
// jump to the Trace tab and run a trace for the table (reuses the trace panel).
function traceTable(name){
 tab("trace");if(window.loadWorkspace)loadWorkspace();
 var inp=el("trsym");if(inp){inp.value=name;}
 if(typeof runTrace==="function")runTrace();
}
