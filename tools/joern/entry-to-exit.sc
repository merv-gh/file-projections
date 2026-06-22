// entry-to-exit.sc
//
// All control flows from entrypoints to exitpoints over the call graph. An entrypoint is
// a method whose annotation matches `entry`; an exitpoint is a call whose code matches
// `exit`. For every (entrypoint, exitpoint) pair where the exit's enclosing method is
// reachable from the entrypoint via the call graph, emit a block with a best-effort call
// chain. Defaults are all-to-all; narrow with `entryName` / `exitFile` to go 1-to-1.
//
// joern --script tools/joern/entry-to-exit.sc \
//   --param root=src --param entry=@(KafkaListener|Scheduled|PostMapping|GetMapping) \
//   --param exit='\.(save|send)\s*\(' --param out=.../e2e.jsonl [--param cpgPath=.../cpg.bin]

import java.nio.file.{Files, Paths}
import java.nio.charset.StandardCharsets
import io.shiftleft.codepropertygraph.generated.nodes.Method

@main def exec(root: String, entry: String, exit: String, out: String,
               entryName: String = "", exitFile: String = "", cpgPath: String = "",
               maxPairs: String = "200"): Unit = {

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

  val entryRe = entry.r
  val exitRe = exit.r
  val cap = maxPairs.toIntOption.getOrElse(200)

  // Entrypoints: methods carrying a matching annotation (optionally a specific name).
  val entryMethods = cpg.method
    .filter(m => m.annotation.code.exists(c => entryRe.findFirstIn(c).isDefined))
    .filter(m => entryName.isEmpty || m.name == entryName)
    .l
  val entryById = entryMethods.map(m => m.id -> m).toMap

  // Exitpoints: call sites whose code matches (optionally restricted to a file).
  val exitCalls = cpg.call
    .filter(c => exitRe.findFirstIn(c.code).isDefined)
    .filter(c => exitFile.isEmpty || c.file.name.exists(_.contains(exitFile)))
    .l

  // BFS over internal callees to recover a representative entry->exit call chain.
  def chain(from: Method, toId: Long): List[String] = {
    val seen = scala.collection.mutable.Set[Long](from.id)
    val q = scala.collection.mutable.Queue[List[Method]](List(from))
    var depth = 0
    while (q.nonEmpty && depth < 5000) {
      depth += 1
      val path = q.dequeue()
      val head = path.head
      if (head.id == toId) return path.reverse.map(_.name)
      head.callee.l.filter(c => !c.isExternal).distinctBy(_.id).foreach { c =>
        if (!seen.contains(c.id)) { seen += c.id; q.enqueue(c :: path) }
      }
    }
    List(from.name, "…", "(callee)")
  }

  // Transitive callers of a method via node-level BFS (repeat is traversal-only).
  def reachingEntries(exitMethod: Method): List[Method] = {
    val seen = scala.collection.mutable.Set[Long](exitMethod.id)
    val q = scala.collection.mutable.Queue[Method](exitMethod)
    val acc = scala.collection.mutable.ArrayBuffer[Method]()
    if (entryById.contains(exitMethod.id)) acc += exitMethod
    var steps = 0
    while (q.nonEmpty && steps < 5000) {
      steps += 1
      val m = q.dequeue()
      m.caller.l.distinctBy(_.id).foreach { c =>
        if (!seen.contains(c.id)) {
          seen += c.id
          if (entryById.contains(c.id)) acc += c
          q.enqueue(c)
        }
      }
    }
    acc.distinctBy(_.id).toList
  }

  val rows = scala.collection.mutable.ArrayBuffer[String]()
  var pairs = 0

  exitCalls.foreach { call =>
    if (pairs < cap) {
      val exitMethod = call.method
      val reaching = reachingEntries(exitMethod)
      val exitFileName = call.file.name.headOption.getOrElse("")
      val exitLine = call.lineNumber.map(_.toString).getOrElse("?")
      reaching.foreach { e =>
        if (pairs < cap) {
          pairs += 1
          val path = chain(e, exitMethod.id)
          val lines = Seq(
            s"entry  ${e.name}  ${e.filename}:${e.lineNumber.map(_.toString).getOrElse("?")}",
            s"chain  ${path.mkString(" -> ")}",
            s"exit   ${call.code.linesIterator.next().take(140)}  ${exitFileName}:${exitLine}"
          )
          val facts = Seq(
            s"entrypoint: ${e.name}",
            s"exitpoint: ${call.code.linesIterator.next().take(140)}",
            s"hops: ${path.size - 1}"
          )
          rows += s"""{"kind":"block","id":${jstr(s"${e.name}->${exitMethod.name}@${exitLine}")},"file":${jstr(exitFileName)},"mode":"entry-to-exit","tool":"joern","lines":${jarr(lines)},"facts":${jarr(facts)}}"""
        }
      }
    }
  }

  if (rows.isEmpty)
    rows += s"""{"kind":"fact","id":"no-flow","tool":"joern","text":${jstr(s"no entry(${entry})->exit(${exit}) control flow found")}}"""

  Files.write(Paths.get(out), rows.mkString("\n").getBytes(StandardCharsets.UTF_8))
}
