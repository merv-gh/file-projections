// object-flow.sc
//
// Object data-flow projection. For a target type, walk every method that ALLOCATES one and
// emit a per-object timeline of how that instance is transformed — direct setters and
// cross-file stage calls (resolved to the field they mutate) — each annotated with the
// branch condition it happens under, plus the fields that are never set (end null/default).
// This separates distinct objects (e.g. a decoy) and distinct branches so a naive search
// can't be fooled. Optional `field` narrows to one field.
//
// joern --script tools/joern/object-flow.sc \
//   --params root=src/main/java,typeName=Receipt,out=.../of.jsonl[,field=tier][,cpgPath=...]

import java.nio.file.{Files, Paths}
import java.nio.charset.StandardCharsets
import io.shiftleft.codepropertygraph.generated.nodes.{Call, Method}

@main def exec(root: String, typeName: String, out: String, cpgPath: String = "", field: String = ""): Unit = {

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

  val fields = cpg.typeDecl.nameExact(typeName).member.name.l.distinct
  val wantField = if (field.isEmpty) "" else field.toLowerCase
  def fieldOf(setter: String): String = setter.stripPrefix("set").toLowerCase

  // Branch conditions a call sits under, within its own method (true branch as written,
  // false branch negated). Empty when unconditional.
  def branchOf(c: Call, m: Method): String = {
    val css = m.controlStructure.filter(cs => cs.ast.id.l.contains(c.id)).l
    css.flatMap { cs =>
      cs.condition.code.headOption.map { cond =>
        if (cs.whenTrue.ast.id.l.contains(c.id)) cond else s"!(${cond})"
      }
    }.distinct.mkString(" && ")
  }

  val rows = scala.collection.mutable.ArrayBuffer[String]()
  val typeRe = ".*" + java.util.regex.Pattern.quote(typeName) + ".*"

  // Allocation sites group the timeline by the concrete object the source builds.
  def allocTrav = cpg.call.nameExact("<init>").where(_.methodFullName(typeRe))
  val scopes = allocTrav.method.dedup.l

  scopes.foreach { m =>
    val objNames = m.local.filter(_.typeFullName.matches(typeRe)).name.toSet
    val alloc = m.ast.isCall.nameExact("<init>").where(_.methodFullName(typeRe)).l.headOption
    val ctorCode = alloc.map(_.code.linesIterator.next()).getOrElse(s"new ${typeName}(...)")
    val ctorLine = alloc.flatMap(_.lineNumber.map(_.toInt)).getOrElse(0)

    // editAt = the exact source location to fix (the setter line itself, in the callee).
    case class Ev(line: Int, branch: String, field: String, what: String, file: String, editAt: String, suspect: String)
    val events = scala.collection.mutable.ArrayBuffer[Ev]()
    val setCount = scala.collection.mutable.Map[String, Int]().withDefaultValue(0)

    m.call.l.sortBy(_.lineNumber.getOrElse(0)).foreach { c =>
      val argNames = c.argument.isIdentifier.name.toSet
      if ((objNames & argNames).nonEmpty) {
        val br = branchOf(c, m)
        val file = c.file.name.headOption.getOrElse("?")
        val line = c.lineNumber.map(_.toInt).getOrElse(0)
        if (c.name.startsWith("set")) {
          val f = fieldOf(c.name); setCount(f) += 1
          events += Ev(line, br, f, c.code.linesIterator.next().take(120), file, s"${file}:${line}", "")
        } else {
          // A stage/helper call taking the object: resolve the callee's setters on it,
          // and point at the exact setter line inside the callee (where the fix goes).
          val setters = c.callee.ast.isCall.name("set.*").l.distinctBy(_.name)
          val stageName = c.callee.typeDecl.name.headOption.getOrElse(c.name)
          setters.foreach { s =>
            val f = fieldOf(s.name); setCount(f) += 1
            val sf = s.file.name.headOption.getOrElse("?")
            val sl = s.lineNumber.map(_.toInt).getOrElse(0)
            // Name/field mismatch: the stage's name mentions a field it does NOT set.
            val susp = fields.find(ff => ff != f && stageName.toLowerCase.contains(ff))
              .map(ff => s"${stageName} sets `${f}` but its name suggests `${ff}`").getOrElse("")
            events += Ev(line, br, f, s"${stageName}.apply(...)  ⇒ ${s.code.linesIterator.next().take(60)}", file, s"${sf}:${sl}", susp)
          }
        }
      }
    }

    val shown = events.filter(e => wantField.isEmpty || e.field == wantField).sortBy(_.line)
    val lines = (s"${alloc.map(_.file.name.headOption.getOrElse("?")).getOrElse("?")}:${ctorLine} :: ${ctorCode}  [object built in ${m.fullName.split(":").head}]" +:
      shown.map { e =>
        val br = if (e.branch.isEmpty) "" else s"  [when ${e.branch}]"
        val flag = if (e.suspect.nonEmpty) s"  ⚠ SUSPECT: ${e.suspect}" else ""
        s"${e.file}:${e.line} :: ${e.what}  (field ${e.field}, edit ${e.editAt})${br}${flag}"
      }).toList
    val missing = fields.filter(f => setCount(f) == 0 && (wantField.isEmpty || f == wantField))
    val multi = setCount.filter { case (f, n) => n > 1 && (wantField.isEmpty || f == wantField) }.keys.toSeq.sorted
    val mismatches = shown.filter(_.suspect.nonEmpty).map(e => s"${e.suspect} @ ${e.editAt}").distinct
    val facts =
      Seq(s"object scope: ${m.fullName.split(":").head}") ++
      (if (missing.nonEmpty) Seq(s"ANOMALY — fields NEVER set (end null): ${missing.mkString(", ")} → some setter call likely targets the wrong field") else Nil) ++
      (if (multi.nonEmpty) Seq(s"ANOMALY — fields set MORE THAN ONCE (suspect): ${multi.mkString(", ")}") else Nil) ++
      mismatches.map("ANOMALY — name/field mismatch: " + _)
    rows += s"""{"kind":"block","id":${jstr(s"object@${m.name}")},"file":${jstr(typeName)},"mode":"object-flow","tool":"joern","lines":${jarr(lines)},"facts":${jarr(facts)}}"""
  }

  if (scopes.isEmpty)
    rows += s"""{"kind":"fact","id":"no-alloc","tool":"joern","text":${jstr(s"no allocation of ${typeName} found")}}"""

  Files.write(Paths.get(out), rows.mkString("\n").getBytes(StandardCharsets.UTF_8))
}
