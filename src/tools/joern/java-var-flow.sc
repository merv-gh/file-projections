// java-var-flow.sc
//
// Joern variable-flow projection adapter. Emits normalized JSONL records that
// file-projections renders. JSON is hand-built (no io.circe) so the script runs on
// stock Joern images where circe is not on the script classpath.
//
// joern --script tools/joern/java-var-flow.sc \
//   --params root=src/main/java,targetFile=org/example/Foo.java,targetVar=owner,targetLine=156,out=.../x.jsonl[,cpg=.../cpg.bin]

import java.nio.file.{Files, Paths}
import java.nio.charset.StandardCharsets

@main def exec(root: String, targetFile: String, targetVar: String, targetLine: String = "0", targetMethod: String = "", out: String, cpgPath: String = ""): Unit = {

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

  // Reuse a prebuilt CPG (from `file-projections build`) when provided; otherwise
  // import the source tree fresh.
  if (cpgPath.nonEmpty && Files.exists(Paths.get(cpgPath))) importCpg(cpgPath)
  else importCode(inputPath = root)

  val lineNum = targetLine.toIntOption.getOrElse(0)

  def targetNodes =
    if (lineNum > 0)
      cpg.identifier.nameExact(targetVar).where(_.file.name(".*" + java.util.regex.Pattern.quote(targetFile) + ".*")).where(_.lineNumber(lineNum))
    else
      cpg.identifier.nameExact(targetVar).where(_.method.nameExact(targetMethod))

  def methods = targetNodes.method.dedup
  def sources = methods.parameter ++ methods.local ++ methods.ast.isIdentifier
  val flows = targetNodes.reachableByFlows(sources).l

  val rows = scala.collection.mutable.ArrayBuffer[String]()

  flows.zipWithIndex.foreach { case (flow, idx) =>
    val lines = flow.elements.map { n =>
      val file = n.file.name.headOption.getOrElse("")
      val line = n.lineNumber.map(_.toString).getOrElse("?")
      s"${file}:${line} :: ${n.code}"
    }
    val facts = Seq(
      s"target variable: ${targetVar}",
      s"target file: ${targetFile}",
      s"target line: ${lineNum}",
      s"joern reachableByFlows path index: ${idx}"
    )
    rows += s"""{"kind":"block","id":${jstr(s"joern-var-flow:${targetVar}:${idx}")},"file":${jstr(targetFile)},"mode":"var-flow","tool":"joern","lines":${jarr(lines)},"facts":${jarr(facts)}}"""
  }

  if (rows.isEmpty) {
    rows += s"""{"kind":"fact","id":"no-flow","tool":"joern","text":${jstr(s"No reachableByFlows results for ${targetVar} in ${targetFile}:${lineNum}")}}"""
  }

  Files.write(Paths.get(out), rows.mkString("\n").getBytes(StandardCharsets.UTF_8))
}
