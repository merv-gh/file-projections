// graph.js — result/graph/config tabs + the service-graph SVG renderer and mermaid export.
var GPANES={result:["tresult","resultpane"],graph:["tgraph","graphpane"],cfg:["tcfg","cfgpane"]};
function tab(name){for(var k in GPANES){var on=k===name;el(GPANES[k][0]).classList.toggle("on",on);el(GPANES[k][1]).style.display=on?"":"none"}}
el("tresult").onclick=function(){tab("result")};el("tcfg").onclick=function(){tab("cfg")};
el("tgraph").onclick=function(){tab("graph");if(!GRAPH)loadGraph()};

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
function graphNodesEdges(g){
 var crossOnly=el("gcross").checked;
 var edges=g.edges,nodes=g.nodes;
 if(crossOnly){
  edges=edges.filter(function(e){return e.cross});
  var keep={};edges.forEach(function(e){keep[e.from]=1;keep[e.to]=1});
  nodes=nodes.filter(function(n){return keep[n.id]});
 }
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
 nodes.forEach(function(n){
  var p=pos[n.id];if(!p)return;
  var label=n.label.length>30?"…"+n.label.slice(-29):n.label;
  var sub=n.op?(" · "+n.op):"";
  svg.push('<g class="gnode k'+n.kind+'" data-id="'+esc(n.id)+'" transform="translate('+p.x+','+p.y+')">'+
   '<rect width="'+NODEW+'" height="'+NODEH+'" rx="5"></rect>'+
   '<text x="7" y="14">'+esc(label)+'</text></g>');
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
