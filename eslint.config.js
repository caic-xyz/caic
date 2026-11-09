// Linting rules and plugin configuration for TypeScript and SolidJS source files.

import eslint from "@eslint/js";
import { defineConfig } from "eslint/config";
import tseslint from "typescript-eslint";
import solid from "eslint-plugin-solid/configs/typescript";
import globals from "globals";
import prettier from "eslint-config-prettier";

const sharedRules = {
  "no-unused-vars": "off",
  "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_", varsIgnorePattern: "^_" }],
  "@typescript-eslint/no-explicit-any": "error",
  "@typescript-eslint/no-non-null-assertion": "error",
  "@typescript-eslint/consistent-type-imports": ["error", { prefer: "type-imports" }],
  "no-shadow": "off",
  "@typescript-eslint/no-shadow": ["error", { hoist: "all" }],
  "no-console": ["error", { allow: ["warn", "error"] }],
  "@typescript-eslint/switch-exhaustiveness-check": "error",
  // node:test owns test registration promises and reports their failures.
  "@typescript-eslint/no-floating-promises": [
    "error",
    { allowForKnownSafeCalls: [{ from: "package", name: ["test", "it", "describe"], package: "@types/node" }] },
  ],
  // Solid event callbacks can return handled promises; the framework does not await them.
  "@typescript-eslint/no-misused-promises": ["error", { checksVoidReturn: { attributes: false } }],
  "@typescript-eslint/no-misused-spread": "error",
  "@typescript-eslint/restrict-template-expressions": ["error", { allowNumber: true }],
  "no-debugger": "error",
  eqeqeq: ["error", "always"],
  "no-var": "error",
  "prefer-const": "error",
  "prefer-arrow-callback": "error",
  "object-shorthand": "error",
};

// Test doubles intentionally use loose external fixtures; production values must be typed.
// no-unnecessary-condition cannot follow Solid lifecycle and cancellation state across callbacks.
const productionRules = {
  "@typescript-eslint/await-thenable": "error",
  "@typescript-eslint/no-array-delete": "error",
  "@typescript-eslint/no-base-to-string": "error",
  "@typescript-eslint/no-deprecated": "error",
  "@typescript-eslint/no-for-in-array": "error",
  "@typescript-eslint/no-unnecessary-type-assertion": "error",
  "@typescript-eslint/no-unsafe-argument": "error",
  "@typescript-eslint/no-unsafe-assignment": "error",
  "@typescript-eslint/no-unsafe-call": "error",
  "@typescript-eslint/no-unsafe-member-access": "error",
  "@typescript-eslint/no-unsafe-return": "error",
  "@typescript-eslint/only-throw-error": "error",
  "@typescript-eslint/prefer-promise-reject-errors": "error",
  "@typescript-eslint/require-await": "error",
  "@typescript-eslint/use-unknown-in-catch-callback-variable": "error",
};

export default defineConfig(
  eslint.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["frontend/src/**/*.{ts,tsx}"],
    ...solid,
    languageOptions: {
      globals: { ...globals.browser },
      parserOptions: { project: "./tsconfig.json" },
    },
    rules: {
      ...sharedRules,
      "solid/components-return-once": "error",
      "solid/event-handlers": "error",
      "solid/imports": "error",
      "solid/jsx-no-duplicate-props": "error",
      "solid/jsx-no-script-url": "error",
      "solid/jsx-no-undef": ["error", { typescriptEnabled: true }],
      "solid/jsx-uses-vars": "error",
      "solid/no-array-handlers": "error",
      "solid/no-destructure": "error",
      "solid/no-innerhtml": "error",
      "solid/no-react-deps": "error",
      "solid/no-react-specific-props": "error",
      "solid/no-unknown-namespaces": "error",
      "solid/prefer-for": "error",
      "solid/reactivity": "error",
      "solid/self-closing-comp": "error",
      "solid/style-prop": "error",
      // The router runs with explicitLinks (index.tsx): it ignores a plain <a href> and the browser
      // reloads the document. Mirror its skip conditions so each anchor states its intent.
      "no-restricted-syntax": [
        "error",
        {
          selector:
            "JSXOpeningElement[name.name='a']:has(> JSXAttribute[name.name='href'])" +
            ":not(:has(> JSXAttribute[name.name=/^(link|target|download)$/]))" +
            ":not(:has(> JSXAttribute[name.name='rel'][value.value=/\\bexternal\\b/]))",
          message:
            'Add `link` for in-app navigation, or `target`, `download`, or rel="external" to leave the app on purpose.',
        },
      ],
    },
  },
  // The Node scripts under scripts/ run outside the browser and reach for process, argv, and the
  // filesystem, so they need the Node globals that the frontend sources must not see.
  {
    files: ["scripts/**/*.mjs"],
    languageOptions: { globals: { ...globals.node } },
  },
  {
    files: ["frontend/src/**/*.tsx"],
    // Solid's ref={element} assigns the variable during rendering.
    rules: { "no-unassigned-vars": "off" },
  },
  {
    files: ["frontend/src/**/*.{ts,tsx}"],
    ignores: ["**/*.test.{ts,tsx}", "**/*.bench.{ts,tsx}"],
    rules: productionRules,
  },
  // scripts/check-staged.sh mirrors these roots so its eslint invocation never receives an
  // ignored path (which would only print an ignore warning). Keep the two lists in sync.
  {
    ignores: [
      "**/*.module.css.d.ts",
      "backend/**",
      "frontend/dist/**",
      "frontend/public/**",
      "sdk/**",
      "test-results/**",
      "visual-screenshots-failures/**",
    ],
  },
  prettier,
);
