import js from '@eslint/js';
import hooks from 'eslint-plugin-react-hooks';
import globals from 'globals';
import tseslint from 'typescript-eslint';

export default [
  { ignores: ['dist/**', 'node_modules/**'] },
  {
    ...js.configs.recommended,
    files: ['eslint.config.js'],
    languageOptions: { globals: globals.node },
  },
  {
    files: ['web/react/**/*.{ts,tsx}', 'vite.config.ts'],
    languageOptions: {
      parser: tseslint.parser,
      ecmaVersion: 'latest',
      sourceType: 'module',
      globals: { ...globals.browser, ...globals.node },
      parserOptions: { ecmaFeatures: { jsx: true } },
    },
    plugins: { 'react-hooks': hooks, '@typescript-eslint': tseslint.plugin },
    rules: {
      ...js.configs.recommended.rules,
      'no-undef': 'off',
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'error',
      'no-unused-vars': 'off',
      '@typescript-eslint/no-unused-vars': [
        'error',
        { varsIgnorePattern: '^[_A-Z]', argsIgnorePattern: '^_' },
      ],
    },
  },
];
