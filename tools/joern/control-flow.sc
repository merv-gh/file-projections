// control-flow.sc
//
// Joern control-flow projection: enumerate acyclic CFG paths from a method's entry to
// a target line, using the real CPG. Unlike the lexical fallback this handles else-if
// chains, switch/case, and loops (loops are traversed acyclically — visited nodes are
// not re-entered, so each path is finite). Emits one JSONL block per path.
//
// joern --script tools/joern/control-flow.sc \
//   --param root=src --param targetFile=org/example/Foo.java --param targetLine=42 \
//   --param out=.../cf.jsonl [--param cpgPath=.../cpg.bin] [--param maxPaths=32]

import java.nio.file.{Files, Paths}
import java.nio.charset.StandardCharsets
import io.shiftleft.codepropertygraph.generated.nodes.{StoredNode, CfgNode}

@main def exec(root: String, targetFile: String, targetLine: String, out: String,
               targetMethod: String = "", cpgPath: String = "",
               maxPaths: String = "32", maxDepth: String = "600"): Unit = {

  def jstr(s: String): String = {
    val b = new StringBuilder("\"")
    s.foreach {
      case '"'  => b.append("\\\"")
      case '\\' => b.append("\\\\")
      case '\n' => b.append("\\n")
      case '\r' => b.append("\\r")
      case '\t' => b.append("\\t")
      case c if c < ' ' => b.append("\\u%04x".format(c.toInt))
      case c    => b.append(c)
    }
    b.append("\"").toString
  }
  def jarr(xs: Seq[String]): String = xs.map(jstr).mkString("[", ",", "]")

  if (cpgPath.nonEmpty && Files.exists(Paths.get(cpgPath))) importCpg(cpgPath)
  else importCode(inputPath = root)

  val line = targetLine.toIntOption.getOrElse(0)
  val maxP = maxPaths.toIntOption.getOrElse(32)
  val maxD = maxDepth.toIntOption.getOrElse(600)

  val methods =
    (if (targetMethod.nonEmpty) cpg.method.nameExact(targetMethod)
     else cpg.method.where(_.file.name(".*" + java.util.regex.Pattern.quote(targetFile) + ".*"))
       .filter(m => m.lineNumber.exists(_ <= line) && m.lineNumberEnd.exists(_ >= line))).l

  val rows = scala.collection.mutable.ArrayBuffer[String]()

  if (methods.isEmpty) {
    rows += s"""{"kind":"fact","id":"no-method","tool":"joern","text":${jstr(s"no method found for ${targetFile}:${line}")}}"""
  } else {
    val method = methods.head
    val targetIds = method.ast.isCfgNode.lineNumber(line).id.toSet
    // Precompute the method's control structures: condition line + true/false branch lines.
    // whenTrue/whenFalse cleanly separate then vs else for IFs (empty for other kinds).
    val controls = method.controlStructure.l.map { cs =>
      val condLines = cs.condition.lineNumber.l.map(_.toInt).toSet
      val trueLines = cs.whenTrue.ast.lineNumber.l.map(_.toInt).toSet -- condLines
      val falseLines = cs.whenFalse.ast.lineNumber.l.map(_.toInt).toSet -- condLines
      (cs.lineNumber.map(_.toString).getOrElse("?"), cs.code.linesIterator.next().take(120), condLines, trueLines, falseLines)
    }
    val paths = scala.collection.mutable.ArrayBuffer[List[CfgNode]]()

    def dfs(node: CfgNode, path: List[CfgNode], visited: Set[Long]): Unit = {
      if (paths.size >= maxP) return
      val np = node :: path
      if (targetIds.contains(node.id)) { paths += np.reverse; return }
      if (np.size > maxD) return
      node.cfgNext.l.foreach { s =>
        if (!visited.contains(s.id)) dfs(s, np, visited + node.id)
      }
    }
    dfs(method, Nil, Set.empty)

    if (paths.isEmpty) {
      rows += s"""{"kind":"fact","id":"no-path","tool":"joern","text":${jstr(s"no CFG path to ${targetFile}:${line} in ${method.name}")}}"""
    }

    paths.zipWithIndex.foreach { case (p, idx) =>
      // Collapse to one entry per source line for readability.
      val lines = p.flatMap { n =>
        n.lineNumber.map(ln => s"${ln} :: ${n.code.linesIterator.next().take(160)}")
      }.foldLeft(List.empty[String]) { (acc, cur) => if (acc.headOption.contains(cur)) acc else cur :: acc }.reverse

      // Guards on this path: a control structure is relevant when its condition line is
      // on the path; "entered" if the path also visits its guarded body, else "skipped".
      val pathLines = p.flatMap(_.lineNumber.map(_.toInt)).toSet
      val guards = controls.flatMap { case (ln, code, condLines, trueLines, falseLines) =>
        if (condLines.exists(pathLines.contains)) {
          val decision =
            if (trueLines.exists(pathLines.contains)) "true"
            else if (falseLines.exists(pathLines.contains)) "false"
            else "false (no else)"
          Some(s"@${ln} ${decision}: ${code}")
        } else None
      }.distinct

      val facts = Seq(
        s"target: ${targetFile}:${line} in ${method.name}",
        s"path index: ${idx}",
        s"guards on path: ${guards.size}"
      ) ++ guards.map("guard: " + _)

      rows += s"""{"kind":"block","id":${jstr(s"${method.name}.path-${idx}")},"file":${jstr(targetFile)},"mode":"cfg-path","tool":"joern","lines":${jarr(lines)},"facts":${jarr(facts)}}"""
    }
  }

  Files.write(Paths.get(out), rows.mkString("\n").getBytes(StandardCharsets.UTF_8))
}
