# Vendored Redoc

| | |
|---|---|
| Package | `redoc` 2.5.4 (npm, MIT; `redoc.LICENSE`) |
| File | `redoc.standalone.js`, from `bundles/redoc.standalone.js` in the npm tarball |
| SHA-256 | `dcaf76612bc4a3fbcc923a8966dee2f6146a5f32e5ce1b6f02dd60cbbf89500b` |
| Third-party notices | `redoc.standalone.js.LICENSE.txt` (from the same tarball) |

`apidocs_test.go` fails if the bundle's SHA-256 differs from the one above.
To upgrade: `npm pack redoc@<version>`, copy the bundle and licence files,
update this table and the constant in the test, and review the diff of the
bundle's release notes. The page loads nothing from a CDN.

Redoc has no "try it out" console. Search is off (`disable-search`) because
it runs in a `blob:` web worker, which the page's Content-Security-Policy
does not allow.
