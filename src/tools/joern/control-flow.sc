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
    // Method signature: first non-blank line of the method source (the declaration).
    val sigLine = method.lineNumber.map(_.toInt).getOrElse(0)
    val sig = method.code.linesIterator.find(_.trim.nonEmpty).getOrElse(method.name).trim
    // Real conditions only (IF / SWITCH): condition line, full condition code, and the
    // lines reached when the branch is taken — so we can render the *active* condition.
    val controls = method.controlStructure
      .filter(cs => Set("IF", "SWITCH").contains(cs.controlStructureType)).l
      .flatMap { cs =>
        cs.lineNumber.map { ln =>
          val condCode = cs.condition.code.l.headOption.getOrElse(cs.code.linesIterator.next()).trim
          val trueLines = cs.whenTrue.ast.lineNumber.l.map(_.toInt).toSet - ln.toInt
          (ln.toInt, condCode.take(160), trueLines)
        }
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

    // Longest code fragment on the target line is the active statement (the exitpoint),
    // not a sub-expression node — avoids the deceptive "kind / 1 / kind == 1" triples.
    val exitCode = method.ast.lineNumber(line).code.l
      .map(_.linesIterator.next()).filter(_.trim.nonEmpty)
      .sortBy(-_.length).headOption.getOrElse("").trim.take(160)

    paths.zipWithIndex.foreach { case (p, idx) =>
      val pathLines = p.flatMap(_.lineNumber.map(_.toInt)).toSet
      // Active conditions on this path, in source order. A condition reads as written when
      // its true-branch is taken, else negated — the real predicate that held on this path.
      val conds = controls.collect {
        case (ln, code, trueLines) if pathLines.contains(ln) =>
          val active = if (trueLines.exists(pathLines.contains)) code else s"!(${code})"
          (ln, active)
      }.distinct.sortBy(_._1)

      // Path = entry signature, the active conditions, then the exitpoint. Each line is
      // "<srcLine>\t<code>"; the Go renderer pads code into a column with file:line.
      val rowLines = (Seq((sigLine, sig)) ++ conds ++ Seq((line, exitCode)))
        .map { case (ln, code) => s"${ln}\t${code}" }

      rows += s"""{"kind":"block","id":${jstr(s"${method.name}.path-${idx}")},"file":${jstr(targetFile)},"mode":"cfg-path","tool":"joern","lines":${jarr(rowLines)},"facts":[]}"""
    }
  }

  Files.write(Paths.get(out), rows.mkString("\n").getBytes(StandardCharsets.UTF_8))
}
