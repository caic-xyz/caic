#!/usr/bin/env node
// Lints frontend CSS tokens, modules, raw colors, and presentation embedded in TSX.
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, statSync } from "node:fs";
import { resolve, dirname, extname } from "node:path";
import postcss from "postcss";
import selectorParser from "postcss-selector-parser";
import valueParser from "postcss-value-parser";
import ts from "typescript";

const CSS_VARIABLE = /^--[A-Za-z][\w-]*$/u;
const HEX_COLOR = /^#[0-9a-f]{3,8}$/iu;
const COLOR_FUNCTIONS = new Set(["rgb", "rgba", "hsl", "hsla", "hwb", "lab", "lch", "oklab", "oklch", "color"]);
// CSS Color's 148 opaque named colors. transparent and currentcolor intentionally stay allowed:
// the former is useful in gradients and the latter inherits a tokenized element color.
const OPAQUE_NAMED_COLORS = new Set(
  `aliceblue antiquewhite aqua aquamarine azure beige bisque black
  blanchedalmond blue blueviolet brown burlywood cadetblue chartreuse chocolate
  coral cornflowerblue cornsilk crimson cyan darkblue darkcyan darkgoldenrod
  darkgray darkgrey darkgreen darkkhaki darkmagenta darkolivegreen darkorange darkorchid
  darkred darksalmon darkseagreen darkslateblue darkslategray darkslategrey darkturquoise darkviolet
  deeppink deepskyblue dimgray dimgrey dodgerblue firebrick floralwhite forestgreen
  fuchsia gainsboro ghostwhite gold goldenrod gray grey green
  greenyellow honeydew hotpink indianred indigo ivory khaki lavender
  lavenderblush lawngreen lemonchiffon lightblue lightcoral lightcyan lightgoldenrodyellow lightgray
  lightgrey lightgreen lightpink lightsalmon lightseagreen lightskyblue lightslategray lightslategrey
  lightsteelblue lightyellow lime limegreen linen magenta maroon mediumaquamarine
  mediumblue mediumorchid mediumpurple mediumseagreen mediumslateblue mediumspringgreen
  mediumturquoise mediumvioletred midnightblue mintcream mistyrose moccasin navajowhite navy oldlace olive
  olivedrab orange orangered orchid palegoldenrod palegreen paleturquoise palevioletred papayawhip peachpuff
  peru pink plum powderblue purple red rebeccapurple rosybrown royalblue saddlebrown salmon sandybrown seagreen
  seashell sienna silver skyblue slateblue slategray slategrey snow springgreen steelblue tan teal thistle tomato
  turquoise violet wheat white whitesmoke yellow yellowgreen`
    .split(/\s+/u)
    .filter(Boolean),
);
const RAW_VARIABLE_USE = /var\(\s*(--[\w-]+)/gu;
const RAW_HEX_COLOR = /(?<!&)#[0-9a-fA-F]{3,8}\b/gu;
const RAW_FUNCTION_COLOR = /\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(([^)]*)\)/gu;
// HTML and JSON do not have a CSS AST, so their color policy needs narrow text matching.
const RAW_NAMED_COLOR = new RegExp(`(?<![-\\w.])(${[...OPAQUE_NAMED_COLORS].sort().join("|")})(?![-\\w])`, "giu");
const HELP = `Usage: node scripts/lint_frontend_styles.mjs [--source-path PATH] [--token-file PATH]

Lint tracked and unignored frontend files.

Options:
  -h, --help              Show this help and exit.
  --source-path PATH      Frontend directory to lint (default: frontend).
  --token-file PATH       CSS file that owns shared design tokens (default: frontend/src/global.css).

Checks:
  CSS custom properties   Each var(--token) must be declared in the shared token file or the same stylesheet.
  Shared CSS tokens       Each token declared in the shared token file must be referenced somewhere in the frontend.
  CSS modules             Every local .module.css class must be default-imported and referenced through that module.
                          Dynamic styles[expression] access is forbidden; use an explicit Record<Variant, string>.
  Raw colors              CSS and HTML may use literal colors only in the shared token file's custom-property declarations.
                          TS/TSX string literals may not contain literal hex or functional colors. transparent and
                          currentcolor are allowed because they derive no fixed palette value.
  JSON colors             Ordinary JSON may not contain literal colors. Palette-only data must be named
                          <Feature>ColorPalette.json; only manifest theme_color and background_color are exempt.
  Inline SVG              TSX may not embed <svg>; use a standalone asset/component that owns its presentation.
                          Data-driven visualizations may use <svg data-generated-svg="">, but still must use CSS
                          module tokens and may not introduce raw colors or inline styles.
  Inline styles           TSX style= may only pass dynamic CSS custom properties to component-owned CSS.
  SVG custom properties   Standalone SVG assets may not depend on CSS custom properties.
  SVG imports             Every SVG imported by TS/TSX must choose an explicit representation: ?solid inlines a
                          styleable Solid component; ?url imports an opaque asset URL for <img> or CSS, which Vite may
                          inline or emit as a hashed file.
`;

function parseArguments(argv) {
  let sourcePath = "frontend";
  let tokenFile = "frontend/src/global.css";
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "-h" || argument === "--help") {
      process.stdout.write(HELP);
      return null;
    }
    if (argument === "--source-path" || argument === "--token-file") {
      const value = argv[index + 1];
      if (value === undefined || value.startsWith("-")) {
        throw new Error(`Missing value for ${argument}\n${HELP}`);
      }
      if (argument === "--source-path") {
        sourcePath = value;
      } else {
        tokenFile = value;
      }
      index += 1;
      continue;
    }
    throw new Error(`Unknown argument: ${argument}\n${HELP}`);
  }
  return { sourcePath, tokenFile };
}

function sourceLine(node) {
  return node.source?.start?.line ?? 1;
}

function sourceFileLine(sourceFile, node) {
  return sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line + 1;
}

function readText(path) {
  return readFileSync(path, "utf8");
}

function parseCss(path) {
  // PostCSS preserves declaration structure and locations, avoiding matches in comments and strings.
  return postcss.parse(readText(path), { from: path });
}

function cssVariableUses(value) {
  const variables = new Set();
  valueParser(value).walk((node) => {
    if (node.type !== "function" || node.value.toLowerCase() !== "var") {
      return;
    }
    // var() may have whitespace, a fallback, and nested functions; only its first argument names the token.
    const firstArgument = node.nodes.find((child) => child.type !== "comment" && child.type !== "space");
    if (firstArgument?.type === "word" && CSS_VARIABLE.test(firstArgument.value)) {
      variables.add(firstArgument.value);
    }
  });
  return variables;
}

function rawVariableUseLines(text) {
  return text
    .split("\n")
    .flatMap((line, index) =>
      [...line.matchAll(RAW_VARIABLE_USE)].map((match) => ({ line: index + 1, variable: match[1] })),
    );
}

function cssDefinitions(root) {
  const definitions = new Map();
  root.walkDecls((declaration) => {
    if (CSS_VARIABLE.test(declaration.prop)) {
      definitions.set(declaration.prop, sourceLine(declaration));
    }
  });
  return definitions;
}

function cssUses(root) {
  const uses = [];
  root.walkDecls((declaration) => {
    for (const variable of cssVariableUses(declaration.value)) {
      uses.push({ line: sourceLine(declaration), variable });
    }
  });
  return uses;
}

function isGlobalClass(classNode) {
  // :global() selectors belong outside this CSS module and therefore have no local-use requirement.
  for (let parent = classNode.parent; parent !== undefined; parent = parent.parent) {
    if (parent.type === "pseudo" && parent.value === ":global") {
      return true;
    }
  }
  return false;
}

function cssClasses(root, path) {
  const classes = new Set();
  root.walkRules((rule) => {
    try {
      selectorParser((selectors) => {
        selectors.walkClasses((classNode) => {
          if (!isGlobalClass(classNode)) {
            classes.add(classNode.value);
          }
        });
      }).processSync(rule.selector);
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error);
      throw new Error(`${path}:${sourceLine(rule)}: invalid CSS selector: ${detail}`, { cause: error });
    }
  });
  return classes;
}

function cssCustomPropertyContracts(root, path) {
  // Index the custom properties that each local class both defaults and consumes. A selector branch qualifies only
  // when one local class, optionally with a non-functional pseudo-class or pseudo-element, is sufficient to match it.
  // Descendant, compound, tag, attribute, and functional-pseudo selectors require context that a class reference on
  // the JSX element does not prove, so they cannot authorize an inline runtime input.
  const contracts = new Map();
  root.walkRules((rule) => {
    const classes = new Set();
    try {
      selectorParser((selectors) => {
        selectors.each((selector) => {
          const selectorClasses = [];
          let standaloneClass = true;
          selector.walk((node) => {
            if (node.type === "class" && !isGlobalClass(node)) {
              selectorClasses.push(node.value);
              return;
            }
            if (["attribute", "combinator", "id", "nesting", "tag", "universal"].includes(node.type)) {
              standaloneClass = false;
              return;
            }
            if (node.type === "pseudo" && node.nodes !== undefined) {
              standaloneClass = false;
            }
          });
          if (standaloneClass && selectorClasses.length === 1) {
            classes.add(selectorClasses[0]);
          }
        });
      }).processSync(rule.selector);
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error);
      throw new Error(`${path}:${sourceLine(rule)}: invalid CSS selector: ${detail}`, { cause: error });
    }
    if (classes.size === 0) {
      return;
    }
    const definitions = new Set();
    const uses = new Set();
    rule.walkDecls((declaration) => {
      if (CSS_VARIABLE.test(declaration.prop)) {
        definitions.add(declaration.prop);
      }
      for (const variable of cssVariableUses(declaration.value)) {
        uses.add(variable);
      }
    });
    for (const className of classes) {
      const contract = contracts.get(className) ?? { definitions: new Set(), uses: new Set() };
      definitions.forEach((variable) => contract.definitions.add(variable));
      uses.forEach((variable) => contract.uses.add(variable));
      contracts.set(className, contract);
    }
  });
  return contracts;
}

function isRawColorFunction(node) {
  // A color function that incorporates var() still derives its appearance from a design token.
  return (
    node.type === "function" &&
    COLOR_FUNCTIONS.has(node.value.toLowerCase()) &&
    !valueParser.stringify(node.nodes).includes("var(")
  );
}

function colorsInCssValue(value, checkNamedColors) {
  const colors = new Set();
  valueParser(value).walk((node) => {
    if (node.type === "word") {
      if (HEX_COLOR.test(node.value)) {
        colors.add(node.value.toLowerCase());
      }
      if (checkNamedColors && OPAQUE_NAMED_COLORS.has(node.value.toLowerCase())) {
        colors.add(node.value.toLowerCase());
      }
    }
    if (isRawColorFunction(node)) {
      colors.add(`${node.value.toLowerCase()}(${valueParser.stringify(node.nodes).replaceAll(/\s+/gu, "")})`);
    }
  });
  return colors;
}

function hardcodedCssColors(path, root, tokenPath) {
  const errors = [];
  root.walkDecls((declaration) => {
    // The shared token file is the sole CSS location allowed to introduce a literal palette value.
    if (resolve(path) === tokenPath && CSS_VARIABLE.test(declaration.prop)) {
      return;
    }
    for (const color of colorsInCssValue(declaration.value, true)) {
      errors.push({ path, line: sourceLine(declaration), value: color });
    }
  });
  return errors;
}

function hardcodedTextColors(path, text, checkNamedColors) {
  const errors = [];
  const lines = text.split("\n");
  for (const [index, line] of lines.entries()) {
    const colors = new Set();
    for (const match of line.matchAll(RAW_HEX_COLOR)) {
      colors.add(match[0].toLowerCase());
    }
    for (const match of line.matchAll(RAW_FUNCTION_COLOR)) {
      if (!match[2].includes("var(")) {
        colors.add(`${match[1].toLowerCase()}(${match[2].replaceAll(/\s+/gu, "")})`);
      }
    }
    if (checkNamedColors) {
      for (const match of line.matchAll(RAW_NAMED_COLOR)) {
        colors.add(match[1].toLowerCase());
      }
    }
    for (const color of colors) {
      errors.push({ path, line: index + 1, value: color });
    }
  }
  return errors;
}

function stringLiteralText(node) {
  // TypeScript's AST excludes comments, so only executable string/template fragments reach text-based checks.
  if (
    ts.isStringLiteral(node) ||
    ts.isNoSubstitutionTemplateLiteral(node) ||
    ts.isTemplateHead(node) ||
    ts.isTemplateMiddle(node) ||
    ts.isTemplateTail(node)
  ) {
    return node.text;
  }
  return null;
}

function hasJsxAttribute(node, name) {
  return node.attributes.properties.some((property) => ts.isJsxAttribute(property) && property.name.text === name);
}

function propertyNameText(name) {
  return ts.isIdentifier(name) || ts.isStringLiteral(name) || ts.isNumericLiteral(name) ? name.text : null;
}

function unwrapExpression(expression) {
  let current = expression;
  while (
    ts.isParenthesizedExpression(current) ||
    ts.isAsExpression(current) ||
    ts.isTypeAssertionExpression(current) ||
    ts.isNonNullExpression(current) ||
    ts.isSatisfiesExpression(current)
  ) {
    current = current.expression;
  }
  return current;
}

// Recognize style={{ '--component-input': value }} as a candidate dynamic-style boundary. The caller additionally
// verifies that a CSS-module class on this element defines a fallback and consumes every property. Calls, spreads,
// computed keys, ordinary CSS properties, empty objects, and literal values stay forbidden because the linter cannot
// prove that they preserve the boundary between runtime inputs and stylesheet-owned presentation.
function dynamicCustomPropertyNames(attribute) {
  const expression = ts.isJsxExpression(attribute.initializer) ? attribute.initializer.expression : null;
  if (expression === null || !ts.isObjectLiteralExpression(expression) || expression.properties.length === 0) {
    return null;
  }
  const names = new Set();
  for (const property of expression.properties) {
    if (!ts.isPropertyAssignment(property)) {
      return null;
    }
    const name = propertyNameText(property.name);
    if (name === null || !CSS_VARIABLE.test(name)) {
      return null;
    }
    const value = unwrapExpression(property.initializer);
    if (
      ts.isStringLiteral(value) ||
      ts.isNumericLiteral(value) ||
      ts.isNoSubstitutionTemplateLiteral(value) ||
      value.kind === ts.SyntaxKind.TrueKeyword ||
      value.kind === ts.SyntaxKind.FalseKeyword ||
      value.kind === ts.SyntaxKind.NullKeyword
    ) {
      return null;
    }
    names.add(name);
  }
  return names;
}

function cssModuleClassReferences(node, moduleImports) {
  // Collect direct module-class references from this opening tag's class and classList expressions. Walking those
  // attribute expressions covers templates and classList objects while keeping ownership tied to the styled element;
  // references on ancestors, descendants, or computed module keys cannot prove that the owning CSS rule matches.
  const aliases = new Set(moduleImports.map((moduleImport) => moduleImport.alias));
  const references = [];
  for (const attribute of node.attributes.properties) {
    if (
      !ts.isJsxAttribute(attribute) ||
      !["class", "classList"].includes(attribute.name.text) ||
      attribute.initializer === undefined
    ) {
      continue;
    }
    const visit = (child) => {
      if (
        ts.isPropertyAccessExpression(child) &&
        ts.isIdentifier(child.expression) &&
        aliases.has(child.expression.text)
      ) {
        references.push({ alias: child.expression.text, className: child.name.text });
      }
      ts.forEachChild(child, visit);
    };
    visit(attribute.initializer);
  }
  return references;
}

function isSvgModuleSpecifier(moduleSpecifier) {
  return /\.svg(?:[?#].*)?$/u.test(moduleSpecifier);
}

function isExplicitSvgModuleSpecifier(moduleSpecifier) {
  return moduleSpecifier.endsWith(".svg?solid") || moduleSpecifier.endsWith(".svg?url");
}

function cssModuleImports(sourceFile, path) {
  // Only the default CSS-module import creates the styles.foo namespace used by this policy.
  const imports = [];
  for (const statement of sourceFile.statements) {
    if (
      !ts.isImportDeclaration(statement) ||
      !ts.isStringLiteral(statement.moduleSpecifier) ||
      statement.importClause?.name === undefined
    ) {
      continue;
    }
    const importedPath = statement.moduleSpecifier.text;
    if (importedPath.endsWith(".module.css")) {
      imports.push({ alias: statement.importClause.name.text, path: resolve(dirname(path), importedPath) });
    }
  }
  return imports;
}

function typeScriptAnalysis(path) {
  const text = readText(path);
  const sourceFile = ts.createSourceFile(
    path,
    text,
    ts.ScriptTarget.Latest,
    true,
    path.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS,
  );
  const colors = [];
  const inlineSvg = [];
  const inlineStyles = [];
  const ambiguousSvgImports = [];
  const variableUses = [];
  const moduleImports = cssModuleImports(sourceFile, path);
  const styleUses = new Map(moduleImports.map((moduleImport) => [moduleImport.alias, new Set()]));
  const dynamicStyleUses = [];
  const customPropertyStyles = [];

  for (const statement of sourceFile.statements) {
    if (!ts.isImportDeclaration(statement) || !ts.isStringLiteral(statement.moduleSpecifier)) {
      continue;
    }
    const moduleSpecifier = statement.moduleSpecifier.text;
    if (isSvgModuleSpecifier(moduleSpecifier) && !isExplicitSvgModuleSpecifier(moduleSpecifier)) {
      ambiguousSvgImports.push({
        path,
        line: sourceFileLine(sourceFile, statement.moduleSpecifier),
        value: moduleSpecifier,
      });
    }
  }

  // One AST walk gathers TSX presentation rules and CSS-module references without treating source text as markup.
  const visit = (node) => {
    const literal = stringLiteralText(node);
    if (literal !== null) {
      const literalLine = sourceFileLine(sourceFile, node);
      colors.push(
        ...hardcodedTextColors(path, literal, false).map((finding) => ({
          ...finding,
          line: literalLine + finding.line - 1,
        })),
      );
      for (const use of rawVariableUseLines(literal)) {
        variableUses.push({ line: literalLine + use.line - 1, variable: use.variable });
      }
    }
    if (ts.isPropertyAccessExpression(node) && ts.isIdentifier(node.expression)) {
      const classes = styleUses.get(node.expression.text);
      if (classes !== undefined) {
        classes.add(node.name.text);
      }
    }
    if (ts.isElementAccessExpression(node) && ts.isIdentifier(node.expression)) {
      const classes = styleUses.get(node.expression.text);
      if (classes !== undefined) {
        if (
          node.argumentExpression !== undefined &&
          (ts.isStringLiteral(node.argumentExpression) || ts.isNoSubstitutionTemplateLiteral(node.argumentExpression))
        ) {
          classes.add(node.argumentExpression.text);
        } else {
          // A dynamic key cannot prove a CSS class is used, so require an explicit variant-to-class map.
          dynamicStyleUses.push({ alias: node.expression.text, line: sourceFileLine(sourceFile, node) });
        }
      }
    }
    if (ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) {
      // Generated charts need native SVG geometry; the marker is a narrow, auditable exception for them.
      if (node.tagName.getText(sourceFile) === "svg" && !hasJsxAttribute(node, "data-generated-svg")) {
        inlineSvg.push({ path, line: sourceFileLine(sourceFile, node), value: "<svg>" });
      }
      for (const property of node.attributes.properties) {
        if (ts.isJsxAttribute(property) && property.name.text === "style") {
          const variables = dynamicCustomPropertyNames(property);
          if (variables === null) {
            inlineStyles.push({ path, line: sourceFileLine(sourceFile, property), value: "style" });
          } else {
            customPropertyStyles.push({
              classReferences: cssModuleClassReferences(node, moduleImports),
              line: sourceFileLine(sourceFile, property),
              variables,
            });
          }
        }
      }
    }
    if (
      ts.isCallExpression(node) &&
      node.expression.kind === ts.SyntaxKind.ImportKeyword &&
      ts.isStringLiteral(node.arguments[0])
    ) {
      const moduleSpecifier = node.arguments[0].text;
      if (isSvgModuleSpecifier(moduleSpecifier) && !isExplicitSvgModuleSpecifier(moduleSpecifier)) {
        ambiguousSvgImports.push({ path, line: sourceFileLine(sourceFile, node.arguments[0]), value: moduleSpecifier });
      }
    }
    ts.forEachChild(node, visit);
  };

  visit(sourceFile);
  return {
    ambiguousSvgImports,
    colors,
    customPropertyStyles,
    dynamicStyleUses,
    inlineStyles,
    inlineSvg,
    moduleImports,
    styleUses,
    variableUses,
  };
}

function sourceFiles(sourcePath) {
  // Include tracked work and local additions, while ignoring build outputs and other ignored files.
  const output = execFileSync("git", ["ls-files", "--cached", "--others", "--exclude-standard", sourcePath], {
    encoding: "utf8",
  });
  return output
    .split("\n")
    .filter(Boolean)
    .filter((path) => existsSync(path) && statSync(path).isFile())
    .sort();
}

function printErrors(header, errors, format) {
  if (errors.length === 0) {
    return false;
  }
  process.stdout.write(`${header}\n`);
  for (const error of errors.toSorted(format)) {
    process.stdout.write(`  ${error.path}:${error.line}: ${error.value}\n`);
  }
  return true;
}

function main(argv) {
  const args = parseArguments(argv);
  if (args === null) {
    return 0;
  }
  const files = sourceFiles(args.sourcePath);
  const cssFiles = files.filter((path) => extname(path) === ".css");
  const htmlFiles = files.filter((path) => extname(path) === ".html");
  const typeScriptFiles = files.filter((path) => [".ts", ".tsx"].includes(extname(path)));
  const svgFiles = files.filter((path) => extname(path) === ".svg");
  // A <Feature>ColorPalette.json file is palette-only data. The explicit suffix prevents ordinary JSON
  // configuration from becoming an alternate color-token store.
  const paletteFiles = new Set(files.filter((path) => path.endsWith("ColorPalette.json")));
  // All other JSON must be color-free, apart from the web-manifest fields exempted below.
  const jsonFiles = files.filter((path) => extname(path) === ".json" && !paletteFiles.has(path));
  const tokenPath = resolve(args.tokenFile);
  const cssRoots = new Map(cssFiles.map((path) => [path, parseCss(path)]));
  if (!cssRoots.has(args.tokenFile)) {
    cssRoots.set(args.tokenFile, parseCss(args.tokenFile));
  }
  const typeScript = new Map(typeScriptFiles.map((path) => [path, typeScriptAnalysis(path)]));
  const customPropertyContracts = new Map(
    [...cssRoots]
      .filter(([path]) => path.endsWith(".module.css"))
      .map(([path, root]) => [resolve(path), cssCustomPropertyContracts(root, path)]),
  );
  const sharedDefinitions = cssDefinitions(cssRoots.get(args.tokenFile));
  const variableErrors = [];
  const sharedUses = new Set();

  for (const [path, root] of cssRoots) {
    // A stylesheet can use global tokens and its own local custom-property declarations, but no others.
    const available = new Set([...sharedDefinitions.keys(), ...cssDefinitions(root).keys()]);
    for (const use of cssUses(root)) {
      if (!available.has(use.variable)) {
        variableErrors.push({ path, line: use.line, value: use.variable });
      }
      sharedUses.add(use.variable);
    }
  }
  for (const path of htmlFiles) {
    for (const use of rawVariableUseLines(readText(path))) {
      sharedUses.add(use.variable);
      if (!sharedDefinitions.has(use.variable)) {
        variableErrors.push({ path, line: use.line, value: use.variable });
      }
    }
  }
  for (const [path, analysis] of typeScript) {
    for (const use of analysis.variableUses) {
      sharedUses.add(use.variable);
      if (!sharedDefinitions.has(use.variable)) {
        variableErrors.push({ path, line: use.line, value: use.variable });
      }
    }
  }

  const unusedVariables = [...sharedDefinitions].flatMap(([variable, line]) =>
    sharedUses.has(variable) ? [] : [{ path: args.tokenFile, line, value: variable }],
  );
  const selectorErrors = [];
  for (const [cssPath, root] of cssRoots) {
    if (!cssPath.endsWith(".module.css")) {
      continue;
    }
    const classes = cssClasses(root, cssPath);
    // A CSS module may have multiple importers; a class is live when any importer references it.
    const importers = [...typeScript.entries()].flatMap(([path, analysis]) =>
      analysis.moduleImports
        .filter((moduleImport) => moduleImport.path === resolve(cssPath))
        .map((moduleImport) => ({ alias: moduleImport.alias, path })),
    );
    if (importers.length === 0) {
      for (const className of classes) {
        selectorErrors.push({ path: cssPath, line: 0, value: `.${className} (not imported)` });
      }
      continue;
    }
    const dynamicUse = importers.flatMap(({ alias, path }) =>
      typeScript
        .get(path)
        .dynamicStyleUses.filter((use) => use.alias === alias)
        .map((use) => ({ alias, path, line: use.line })),
    )[0];
    if (dynamicUse !== undefined) {
      selectorErrors.push({
        path: dynamicUse.path,
        line: dynamicUse.line,
        value: `dynamic CSS module access \`${dynamicUse.alias}[...]\`: use an explicit Record<Variant, string> map instead`,
      });
      continue;
    }
    const used = new Set(importers.flatMap(({ alias, path }) => [...typeScript.get(path).styleUses.get(alias)]));
    for (const className of classes) {
      if (!used.has(className)) {
        selectorErrors.push({ path: cssPath, line: 0, value: `.${className}` });
      }
    }
  }

  const colorErrors = [
    ...[...cssRoots].flatMap(([path, root]) => hardcodedCssColors(path, root, tokenPath)),
    ...htmlFiles.flatMap((path) => {
      // Browsers cannot resolve CSS custom properties in theme-color metadata, so its content must remain literal.
      const text = readText(path).replace(/<meta\b[^>]*\bname=["']theme-color["'][^>]*>/giu, "");
      return hardcodedTextColors(path, text, true);
    }),
    ...[...typeScript.values()].flatMap((analysis) => analysis.colors),
  ];
  const jsonColorErrors = jsonFiles.flatMap((path) => {
    // Web app manifests cannot resolve CSS custom properties, so their standard color fields must remain literal.
    const text = readText(path).replace(/^\s*"(?:background_color|theme_color)"\s*:\s*"[^"]*",?\s*$/gmu, "");
    return hardcodedTextColors(path, text, false);
  });
  const inlineSvgErrors = [...typeScript.values()].flatMap((analysis) => analysis.inlineSvg);
  const inlineStyleErrors = [...typeScript.entries()].flatMap(([path, analysis]) => [
    ...analysis.inlineStyles,
    ...analysis.customPropertyStyles.flatMap((style) =>
      [...style.variables].flatMap((variable) => {
        const owned = style.classReferences.some((reference) => {
          const modulePath = analysis.moduleImports.find(
            (moduleImport) => moduleImport.alias === reference.alias,
          )?.path;
          const contract =
            modulePath === undefined ? undefined : customPropertyContracts.get(modulePath)?.get(reference.className);
          return contract?.definitions.has(variable) === true && contract.uses.has(variable);
        });
        return owned
          ? []
          : [{ path, line: style.line, value: `${variable} (missing a CSS-module class with a fallback and use)` }];
      }),
    ),
  ]);
  const ambiguousSvgImportErrors = [...typeScript.values()].flatMap((analysis) => analysis.ambiguousSvgImports);
  const svgCustomPropertyErrors = svgFiles.flatMap((path) =>
    rawVariableUseLines(readText(path)).map((use) => ({ path, line: use.line, value: use.variable })),
  );

  let failed = false;
  failed =
    printErrors(
      "Error: undefined CSS custom properties. Reuse a shared token or define a local custom property before using it:",
      variableErrors,
      (left, right) =>
        left.path.localeCompare(right.path) || left.line - right.line || left.value.localeCompare(right.value),
    ) || failed;
  failed =
    printErrors(
      "Error: unused shared CSS custom properties. Remove each token or reference it with var(...):",
      unusedVariables,
      (left, right) => left.line - right.line || left.value.localeCompare(right.value),
    ) || failed;
  if (selectorErrors.length > 0) {
    process.stdout.write(
      "Error: CSS module selector issues. Remove unused classes or import and reference them through the module:\n",
    );
    for (const error of selectorErrors.toSorted(
      (left, right) =>
        left.path.localeCompare(right.path) || left.line - right.line || left.value.localeCompare(right.value),
    )) {
      process.stdout.write(
        error.line === 0 ? `  ${error.path}: ${error.value}\n` : `  ${error.path}:${error.line}: ${error.value}\n`,
      );
    }
    failed = true;
  }
  failed =
    printErrors(
      `Error: hardcoded color values. Replace each with var(--token); add a shared token in ${args.tokenFile} only when needed:`,
      colorErrors,
      (left, right) =>
        left.path.localeCompare(right.path) || left.line - right.line || left.value.localeCompare(right.value),
    ) || failed;
  failed =
    printErrors(
      "Error: raw JSON colors. Ordinary JSON must not contain colors; put palette-only data in <Feature>ColorPalette.json. " +
        "Only manifest theme_color and background_color are exempt:",
      jsonColorErrors,
      (left, right) =>
        left.path.localeCompare(right.path) || left.line - right.line || left.value.localeCompare(right.value),
    ) || failed;
  failed =
    printErrors(
      'Error: inline SVG roots. Move each SVG to a standalone asset/component; only data-driven visualizations may use <svg data-generated-svg="">:',
      inlineSvgErrors,
      (left, right) => left.path.localeCompare(right.path) || left.line - right.line,
    ) || failed;
  failed =
    printErrors(
      "Error: invalid inline styles. Move static presentation to a component CSS module; style= may only pass dynamic " +
        "CSS custom properties that a class on the same element defines with a fallback and consumes:",
      inlineStyleErrors,
      (left, right) => left.path.localeCompare(right.path) || left.line - right.line,
    ) || failed;
  failed =
    printErrors(
      "Error: SVG assets using CSS custom properties. Give the SVG its own presentation instead of depending on page CSS:",
      svgCustomPropertyErrors,
      (left, right) =>
        left.path.localeCompare(right.path) || left.line - right.line || left.value.localeCompare(right.value),
    ) || failed;
  failed =
    printErrors(
      "Error: SVG imports must choose a representation. Use ?solid to inline a styleable Solid component, " +
        "or ?url to import an opaque asset URL for <img> or CSS; Vite may inline it or emit a hashed file:",
      ambiguousSvgImportErrors,
      (left, right) =>
        left.path.localeCompare(right.path) || left.line - right.line || left.value.localeCompare(right.value),
    ) || failed;
  return failed ? 1 : 0;
}

try {
  process.exitCode = main(process.argv.slice(2));
} catch (error) {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
  process.exitCode = 1;
}
