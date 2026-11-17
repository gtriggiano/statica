# Changelog

## [1.2.0](https://github.com/gtriggiano/statica/compare/v1.1.0...v1.2.0) (2026-11-17)


### Features

* answer HTML redirect pages of index objects with redirects ([#18](https://github.com/gtriggiano/statica/issues/18)) ([4d2b740](https://github.com/gtriggiano/statica/commit/4d2b7403828039874442c93a18ff2007fdb987b1))
* redirect objects that declare x-amz-website-redirect-location ([#16](https://github.com/gtriggiano/statica/issues/16)) ([10a70b2](https://github.com/gtriggiano/statica/commit/10a70b2f08c8e4b9d08e585a99c20dca7467de7e))

## [1.1.0](https://github.com/gtriggiano/statica/compare/v1.0.0...v1.1.0) (2026-11-12)


### Features

* **cache:** add cache.ttl_mode and cache.request_directives ([#10](https://github.com/gtriggiano/statica/issues/10)) ([0353a29](https://github.com/gtriggiano/statica/commit/0353a2979f9e579c78f508685ed5e618dd710d22))
* **config:** add a regex matcher to cache and cache_control rules ([#11](https://github.com/gtriggiano/statica/issues/11)) ([2f84a46](https://github.com/gtriggiano/statica/commit/2f84a4641aa8e16fd3ca985978474280a57c5ed0))


### Bug Fixes

* **redirect:** reject capture patterns that close their anchoring group ([#12](https://github.com/gtriggiano/statica/issues/12)) ([a34d4b8](https://github.com/gtriggiano/statica/commit/a34d4b8227a6dc538bb0fca215df96dc6318fc4a))


### Dependencies

* update go to 1.27.4 ([#8](https://github.com/gtriggiano/statica/issues/8)) ([889f7ed](https://github.com/gtriggiano/statica/commit/889f7ede3987383eb289a2804747893030a4aa38))

## 1.0.0 (2026-11-11)


### Features

* publish release archives and a multi-architecture container image ([1ca7867](https://github.com/gtriggiano/statica/commit/1ca78674b4c7112792638d2eeef998e6f1d49e14))
* serve static websites from S3 ([d24a46d](https://github.com/gtriggiano/statica/commit/d24a46d2e0da3c641968f567cf1adfd9b68e62e6))
