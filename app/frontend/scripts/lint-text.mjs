#!/usr/bin/env node
// lint:text (WP9) — fails when a JSX text node, or a string literal
// rendered as a JSX child, contains a snake_case token ([a-z]+_[a-z]+).
// This catches raw API/enum values (e.g. "not_found", "hw_mode") leaking
// into user-visible copy instead of going through lib/format.ts helpers.
//
// Scope: only actual JSX children — plain JsxText nodes, and string /
// template literals that appear as (or inside) a JSX expression child
// ({...}) of an element or fragment. JSX *attribute* values (className,
// aria-label, or any other prop) and plain TypeScript code (object keys,
// function-call arguments, API field names, comments, imports) are not
// JSX children and are intentionally excluded — snake_case is normal
// there (API field names, Tailwind arbitrary-value tokens, etc).
//
// Implementation uses the TypeScript compiler API (already a project
// dependency) to parse real ASTs rather than guessing with regexes over
// raw source text, so it doesn't misfire on comments, imports, or
// attribute strings.
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import ts from 'typescript'

const ROOT = join(import.meta.dirname, '..')
const SRC = join(ROOT, 'src')
const SNAKE_CASE = /[a-z]+_[a-z]+/

function listTsxFiles(dir) {
  const out = []
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    const s = statSync(full)
    if (s.isDirectory()) {
      out.push(...listTsxFiles(full))
    } else if (entry.endsWith('.tsx')) {
      out.push(full)
    }
  }
  return out
}

// isJsxChildString reports whether `node` (a string/template literal) sits
// inside a JSX child expression ({...} between tags), as opposed to a JSX
// attribute value ({...} inside an opening tag) or ordinary TS code.
function isJsxChildString(node) {
  let current = node
  while (current) {
    if (current.kind === ts.SyntaxKind.JsxAttribute) return false
    if (
      current.kind === ts.SyntaxKind.JsxExpression &&
      current.parent &&
      (current.parent.kind === ts.SyntaxKind.JsxElement ||
        current.parent.kind === ts.SyntaxKind.JsxFragment)
    ) {
      return true
    }
    current = current.parent
  }
  return false
}

function checkFile(filePath) {
  const text = readFileSync(filePath, 'utf8')
  const sourceFile = ts.createSourceFile(filePath, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
  const findings = []

  function reportIfSnakeCase(value, node) {
    const match = value.match(SNAKE_CASE)
    if (!match) return
    const { line } = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile))
    findings.push({ line: line + 1, token: match[0], snippet: value.trim().slice(0, 80) })
  }

  function visit(node) {
    if (node.kind === ts.SyntaxKind.JsxText) {
      const value = node.getText(sourceFile)
      if (value.trim().length > 0) reportIfSnakeCase(value, node)
    } else if (node.kind === ts.SyntaxKind.StringLiteral && isJsxChildString(node)) {
      // .text is the decoded literal value, not the raw source (which
      // would include surrounding quotes but no embedded expressions).
      reportIfSnakeCase(node.text, node)
    } else if (node.kind === ts.SyntaxKind.NoSubstitutionTemplateLiteral && isJsxChildString(node)) {
      reportIfSnakeCase(node.text, node)
    } else if (node.kind === ts.SyntaxKind.TemplateExpression && isJsxChildString(node)) {
      // Only check the literal text spans (the parts actually rendered
      // verbatim), never the ${...} embedded expressions — those are code
      // (property accesses, identifiers), not user-visible copy.
      reportIfSnakeCase(node.head.text, node.head)
      for (const span of node.templateSpans) {
        reportIfSnakeCase(span.literal.text, span.literal)
      }
    }
    ts.forEachChild(node, visit)
  }

  visit(sourceFile)
  return findings
}

const files = listTsxFiles(SRC)
let failed = false

for (const file of files) {
  const findings = checkFile(file)
  for (const f of findings) {
    failed = true
    console.error(`${relative(ROOT, file)}:${f.line}: snake_case token "${f.token}" in JSX text: ${f.snippet}`)
  }
}

if (failed) {
  console.error('\nlint:text failed — replace snake_case tokens in user-visible JSX text with proper copy (see src/lib/format.ts helpers).')
  process.exit(1)
} else {
  console.log(`lint:text OK — scanned ${files.length} file(s), no snake_case tokens in JSX text.`)
}
