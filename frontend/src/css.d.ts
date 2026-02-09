// Type declaration for CSS modules.
declare module "*.module.css" {
  const classes: { readonly [key: string]: string };
  export default classes;
}
