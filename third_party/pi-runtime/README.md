# Pi runtime

Pins the Pi coding agent that the image installs, so the build is
reproducible and every package is checked against a recorded hash.

- Package: `@earendil-works/pi-coding-agent` 1.1.0.
- Integrity: `sha512-SeEi/4hdcHNgA9UWlefZl7ZZpm3dzi2OoxNjDHsBJ9o298LNOtbL4DGKgitlEj6uCTccvtw6f2hlCkTPVJ2RXg==`,
  recorded in `package-lock.json`. Pi no longer ships an `npm-shrinkwrap.json`,
  so this lockfile alone pins its transitive dependencies. Pi depends on its
  sibling `@earendil-works/*` packages with caret ranges, so `package.json`
  pins each of them to the agent's own version in `overrides`.

The Dockerfile copies this directory to `/usr/local/lib/home-agent/pi-runtime`,
runs `npm ci --omit=dev --ignore-scripts` there, and links
`node_modules/.bin/pi` to `/usr/local/bin/pi`. `node_modules` is never committed.

To refresh, bump the version in `package.json` and the `overrides` for every
`@earendil-works/*` sibling to the same version (check them against the
agent's own `package.json`), then regenerate the lockfile in the
runtime base image so the npm version matches:

    docker run --rm -v "$PWD/third_party/pi-runtime:/w" -w /w \
      node:24.19.0-alpine3.24 npm install --package-lock-only --omit=dev --ignore-scripts

then update the version and integrity above and in the Dockerfile `pi --version`
check, rebuild the image, and confirm `pi --version`.
