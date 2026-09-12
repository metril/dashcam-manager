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
// It also fails when a leading piece of user-visible copy starts with a
// lowercase letter: a JSX text node (this covers <label> text too, since
// a label's text is just a JsxText child) that opens its element, or a
// placeholder/title/aria-label attribute string, whose first alphabetic
// character is lowercase. The project's copy rule is sentence case
// ("Make", "No matches"), not lowercase ("make", "no matches").
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

// LOWERCASE_ALLOWLIST is for exact strings that legitimately start with a
// lowercase letter and aren't a copy bug: brand text ("dashcam" in the
// "dashcam.manager" wordmark), units, and literal example values shown in
// input placeholders (hostnames, paths, mount options) rather than labels.
const LOWERCASE_ALLOWLIST = new Set([
  'dashcam', // Layout.tsx wordmark + Shares.tsx share-name placeholder example
  'sedan', // Search.tsx vehicle-type placeholder example
  'nas.local', // Shares.tsx host placeholder example
  'footage', // Shares.tsx share-name placeholder example
  'ro,vers=2.1', // Shares.tsx mount-options placeholder example
  '/mnt/dashcam', // Settings.tsx path placeholder example
  '/mnt/shares/local-test', // Storage.tsx local-storage path placeholder example
  '/dev/dri/renderD128', // Settings.tsx device path placeholder example
  '1080p', // ExportPanel.tsx quality option — a resolution label, not a sentence
  'restart required', // Settings.tsx — the RestartChip badge (rendered uppercase via CSS) and its prose reference
])

const LOWERCASE_ATTRS = new Set(['placeholder', 'title', 'aria-label'])

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

// enclosingJsxExpressionChild walks up from `node` to the nearest
// JsxExpression that is itself a direct child of a JsxElement/JsxFragment
// (i.e. a `{...}` slot between tags), or null if none exists.
function enclosingJsxExpressionChild(node) {
  let current = node
  while (current) {
    if (
      current.kind === ts.SyntaxKind.JsxExpression &&
      current.parent &&
      (current.parent.kind === ts.SyntaxKind.JsxElement || current.parent.kind === ts.SyntaxKind.JsxFragment)
    ) {
      return current
    }
    current = current.parent
  }
  return null
}

// isFirstMeaningfulChild reports whether `node` is the first non-whitespace
// child among its JSX siblings (parent.children, for a JsxElement or
// JsxFragment parent). A node preceded only by other blank JsxText is still
// "first" — but a node preceded by an actual expression or element isn't:
// that's the "starts with a variable" / mid-sentence-continuation case
// ("{activeCount} running", "First seen {date} · last seen {date}"), which
// this lint deliberately doesn't flag.
function isFirstMeaningfulChild(node) {
  const parent = node.parent
  if (!parent || (parent.kind !== ts.SyntaxKind.JsxElement && parent.kind !== ts.SyntaxKind.JsxFragment)) return false
  const children = parent.children
  for (const sibling of children) {
    if (sibling === node) return true
    if (sibling.kind === ts.SyntaxKind.JsxText && sibling.getText().trim() === '') continue
    return false
  }
  return false
}

// stripEntities removes HTML entity references (&rarr;, &middot;, &#8594;)
// before the lowercase-first-letter check, so an entity's spelled-out name
// (itself lowercase, e.g. "rarr") is never mistaken for real copy.
function stripEntities(s) {
  return s.replace(/&#?[a-zA-Z0-9]+;/g, '')
}

// firstAlphaIsLowercase returns true if the first alphabetic character in
// `value` (after stripping HTML entities) is lowercase, or false if it's
// uppercase or there is no alphabetic character at all.
function firstAlphaIsLowercase(value) {
  const stripped = stripEntities(value).trim()
  const m = stripped.match(/[A-Za-z]/)
  if (!m) return false
  const c = m[0]
  return c === c.toLowerCase() && c !== c.toUpperCase()
}

function checkFile(filePath) {
  const text = readFileSync(filePath, 'utf8')
  const sourceFile = ts.createSourceFile(filePath, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX)
  const findings = []

  function lineOf(node) {
    return sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line + 1
  }

  function reportIfSnakeCase(value, node) {
    const match = value.match(SNAKE_CASE)
    if (!match) return
    findings.push({
      line: lineOf(node),
      message: `snake_case token "${match[0]}" in JSX text: ${value.trim().slice(0, 80)}`,
    })
  }

  function reportIfLowercaseStart(value, node) {
    const trimmed = value.trim()
    if (!trimmed) return
    if (LOWERCASE_ALLOWLIST.has(trimmed)) return
    if (!firstAlphaIsLowercase(trimmed)) return
    findings.push({
      line: lineOf(node),
      message: `lowercase-starting text (use sentence case): "${trimmed.slice(0, 80)}"`,
    })
  }

  function visit(node) {
    if (node.kind === ts.SyntaxKind.JsxText) {
      const value = node.getText(sourceFile)
      if (value.trim().length > 0) {
        reportIfSnakeCase(value, node)
        if (isFirstMeaningfulChild(node)) reportIfLowercaseStart(value, node)
      }
    } else if (node.kind === ts.SyntaxKind.StringLiteral && isJsxChildString(node)) {
      // .text is the decoded literal value, not the raw source (which
      // would include surrounding quotes but no embedded expressions).
      reportIfSnakeCase(node.text, node)
      // Only flag a lowercase start when this literal *is* the entire
      // `{...}` slot (not one branch of a ternary/condition buried inside
      // it, e.g. `cond ? 'Foo' : 'all'` or `state === 'queued'`) and that
      // slot leads its element — matches the JsxText handling above.
      const enclosing = enclosingJsxExpressionChild(node)
      if (enclosing && enclosing.expression === node && isFirstMeaningfulChild(enclosing)) {
        reportIfLowercaseStart(node.text, node)
      }
    } else if (node.kind === ts.SyntaxKind.NoSubstitutionTemplateLiteral && isJsxChildString(node)) {
      reportIfSnakeCase(node.text, node)
      const enclosing = enclosingJsxExpressionChild(node)
      if (enclosing && enclosing.expression === node && isFirstMeaningfulChild(enclosing)) {
        reportIfLowercaseStart(node.text, node)
      }
    } else if (node.kind === ts.SyntaxKind.TemplateExpression && isJsxChildString(node)) {
      // Only check the literal text spans (the parts actually rendered
      // verbatim), never the ${...} embedded expressions — those are code
      // (property accesses, identifiers), not user-visible copy.
      reportIfSnakeCase(node.head.text, node.head)
      for (const span of node.templateSpans) {
        reportIfSnakeCase(span.literal.text, span.literal)
      }
      // A template's head text is the only part that can "lead" the
      // rendered string (anything after a ${...} span is definitionally
      // preceded by a variable) — and only when the head is non-empty
      // (a template starting `${x}...` is variable-led from the start).
      const enclosing = enclosingJsxExpressionChild(node)
      if (enclosing && enclosing.expression === node && isFirstMeaningfulChild(enclosing) && node.head.text) {
        reportIfLowercaseStart(node.head.text, node.head)
      }
    } else if (node.kind === ts.SyntaxKind.JsxAttribute) {
      const name = node.name.getText(sourceFile)
      if (
        LOWERCASE_ATTRS.has(name) &&
        node.initializer &&
        node.initializer.kind === ts.SyntaxKind.StringLiteral
      ) {
        reportIfLowercaseStart(node.initializer.text, node.initializer)
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
    console.error(`${relative(ROOT, file)}:${f.line}: ${f.message}`)
  }
}

if (failed) {
  console.error(
    '\nlint:text failed — replace snake_case tokens with proper copy (see src/lib/format.ts helpers), ' +
      'and capitalize lowercase-starting user-visible text to sentence case. ' +
      'A genuine exception (brand text, a unit, a literal placeholder example) goes in LOWERCASE_ALLOWLIST in this script.',
  )
  process.exit(1)
} else {
  console.log(`lint:text OK — scanned ${files.length} file(s).`)
}
