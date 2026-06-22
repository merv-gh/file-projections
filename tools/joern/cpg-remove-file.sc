// cpg-remove-file.sc
//
// Per-node incremental primitive: remove all nodes belonging to one source file from a
// loaded CPG via DiffGraphBuilder — proving on-the-fly .cpg mutation. NOTE: importCpg works
// on a workspace copy, so `close` persists to the workspace, not the original cpg.bin; this
// script demonstrates the node-removal mechanism. A correct node-level re-add additionally
// needs a single-file frontend AST pass plus a whole-program linker re-run to restore
// cross-file edges (call graph / type refs) — see the README "Incremental CPG model".
//
// joern --script tools/joern/cpg-remove-file.sc \
//   --param cpgPath=.../cpg.bin --param file=com/example/shop/Order.java --param out=.../report.txt

import java.nio.file.{Files, Paths}
import java.nio.charset.StandardCharsets

@main def exec(cpgPath: String, file: String, out: String): Unit = {
  importCpg(cpgPath)

  val before = cpg.all.size
  val q = ".*" + java.util.regex.Pattern.quote(file) + ".*"
  val fileNodes = cpg.file.name(q).l
  val methodAst = cpg.method.filter(_.filename.matches(q)).ast.l
  val typeAst = cpg.typeDecl.filter(_.filename.matches(q)).ast.l
  val victims = (fileNodes ++ methodAst ++ typeAst).distinctBy(_.id)

  val dg = Cpg.newDiffGraphBuilder
  victims.foreach(dg.removeNode)
  flatgraph.DiffGraphApplier.applyDiff(cpg.graph, dg)

  val after = cpg.all.size
  val report = s"file=${file}\nremoved_nodes=${before - after}\nnodes_before=${before}\nnodes_after=${after}\n"
  Files.write(Paths.get(out), report.getBytes(StandardCharsets.UTF_8))
  // Persist the mutation back to the CPG store so the removal sticks for later queries.
  close
}
