// The one function the tests use, so the dev dependency needs no types package.
declare module "js-yaml" {
  const yaml: { load(text: string): unknown };
  export default yaml;
}
