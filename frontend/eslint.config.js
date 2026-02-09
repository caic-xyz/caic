import eslint from '@eslint/js';
import tseslint from 'typescript-eslint';
import solid from 'eslint-plugin-solid/configs/typescript';
import globals from 'globals';

const sharedRules = {
  'no-unused-vars': 'off',
  '@typescript-eslint/no-unused-vars': [
    'error',
    { argsIgnorePattern: '^_', varsIgnorePattern: '^_' },
  ],
  '@typescript-eslint/no-explicit-any': 'error',
  '@typescript-eslint/no-non-null-assertion': 'error',
  '@typescript-eslint/consistent-type-imports': [
    'error',
    { prefer: 'type-imports' },
  ],
  'no-shadow': 'off',
  '@typescript-eslint/no-shadow': ['error', { hoist: 'all' }],
  'no-console': ['error', { allow: ['warn', 'error'] }],
  'no-debugger': 'error',
  eqeqeq: ['error', 'always'],
  'no-var': 'error',
  'prefer-const': 'error',
  'prefer-arrow-callback': 'error',
  'object-shorthand': 'error',
};

export default tseslint.config(
  eslint.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ['src/**/*.{ts,tsx}'],
    ...solid,
    languageOptions: {
      globals: { ...globals.browser },
      parserOptions: { project: './tsconfig.json' },
    },
    rules: {
      ...sharedRules,
      'solid/components-return-once': 'error',
      'solid/event-handlers': 'error',
      'solid/imports': 'error',
      'solid/jsx-no-duplicate-props': 'error',
      'solid/jsx-no-script-url': 'error',
      'solid/jsx-no-undef': ['error', { typescriptEnabled: true }],
      'solid/jsx-uses-vars': 'error',
      'solid/no-array-handlers': 'error',
      'solid/no-destructure': 'error',
      'solid/no-innerhtml': 'error',
      'solid/no-react-deps': 'error',
      'solid/no-react-specific-props': 'error',
      'solid/no-unknown-namespaces': 'error',
      'solid/prefer-for': 'error',
      'solid/reactivity': 'error',
      'solid/self-closing-comp': 'error',
      'solid/style-prop': 'error',
    },
  },
  { ignores: ['dist/**', '../sdk/**'] },
);
