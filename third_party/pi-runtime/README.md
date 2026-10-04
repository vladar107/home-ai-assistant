# Pi runtime

Pins the Pi coding agent that the image installs, so the build is
reproducible and every package is checked against a recorded hash.

- Package: `@earendil-works/pi-coding-agent` 1.0.0.
- Integrity: `sha512-/FtbxoSQU/mEv1QnichJjRjqteqaIaMWxmhB4G367+MwZfX7/DI5B9YAg5lqbN7nztFskBEtUSZ+FlmMBECtMw==`,
  recorded in `package-lock.json`. Pi ships its own `npm-shrinkwrap.json`, which
  pins its transitive dependencies.

The Dockerfile copies this directory to `/usr/local/lib/home-agent/pi-runtime`,
runs `npm ci --omit=dev --ignore-scripts` there, and links
`node_modules/.bin/pi` to `/usr/local/bin/pi`. `node_modules` is never committed.

To refresh, bump the version in `package.json`, regenerate the lockfile in the
runtime base image so the npm version matches:

    docker run --rm -v "$PWD/third_party/pi-runtime:/w" -w /w \
      node:24.19.0-alpine3.24 npm install --package-lock-only --omit=dev --ignore-scripts

then update the version and integrity above and in the Dockerfile `pi --version`
check, rebuild the image, and confirm `pi --version`.
