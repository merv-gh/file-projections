// cpg-methods.sc
//
// Language-neutral CPG surface: list methods and their direct call names for a
// Java or Go source root. The caller decides which frontend built the CPG
// (javasrc2cpg/gosrc2cpg); this query only uses common CPG nodes.

import java.nio.file.{Files, Paths}
import java.nio.charset.StandardCharsets

@main def exec(root: String, out: String, cpgPath: String = "", file: String = "", name: String = ""): Unit = {
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

  val rows = scala.collection.mutable.ArrayBuffer[String]()
  val ms = cpg.method
    .filter(m => !m.isExternal)
    .filter(m => file.isEmpty || m.file.name.headOption.exists(_.endsWith(file)))
    .filter(m => name.isEmpty || m.name == name)
    .l
    .sortBy(m => (m.file.name.headOption.getOrElse(""), m.lineNumber.getOrElse(0), m.name))

  ms.foreach { m =>
    val f = m.file.name.headOption.getOrElse("?")
    val line = m.lineNumber.map(_.toInt).getOrElse(0)
    val calls = m.call.name.l.filter(n => !Set("<operator>.assignment", "<operator>.fieldAccess", "<operator>.addition", "<operator>.subtraction").contains(n)).distinct.sorted
    val lines = Seq(s"${m.name} ${f}:${line}", s"calls: ${calls.mkString(", ")}")
    rows += s"""{"kind":"block","id":${jstr(s"${m.name}@${line}")},"file":${jstr(f)},"mode":"methods","tool":"cpg-methods","lines":${jarr(lines)}}"""
  }

  if (ms.isEmpty)
    rows += s"""{"kind":"fact","id":"none","tool":"cpg-methods","text":${jstr("no matching methods found")}}"""

  Files.write(Paths.get(out), rows.mkString("\n").getBytes(StandardCharsets.UTF_8))
}
