* Make internal/manifests/ax-deployment2.yaml the new templated ax-deployment.yaml.tmpl.
* Remove harnesstest package.
* Update HarnessService with the actual protocol.
* Remove axepp.
* Remove ate build tag.
* Remove harnessHandler once Exec RPC is revisited.
* Introduce a SubsrateHarness that wraps a regular Harness implementation and provides
  automatic resume and suspension.
