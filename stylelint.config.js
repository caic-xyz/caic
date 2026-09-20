// Enforces consistent styles for Solid component CSS modules.
export default {
  extends: ["stylelint-config-standard", "stylelint-config-css-modules", "stylelint-config-recess-order"],
  rules: {
    "custom-property-no-missing-var-function": null,
    "selector-class-pattern": null,
  },
};
