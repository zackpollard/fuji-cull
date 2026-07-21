# Changelog

## [1.5.0](https://github.com/zackpollard/fuji-cull/compare/v1.4.0...v1.5.0) (2026-07-21)


### Features

* android app + hardware video decode & perf ([68547c0](https://github.com/zackpollard/fuji-cull/commit/68547c0483d7b86c4e9fad5b88ac4cb0d0f16e9f))

## [1.4.0](https://github.com/zackpollard/fuji-cull/compare/v1.3.0...v1.4.0) (2026-07-13)


### Features

* android usb diagnostics on connect screen, ptp interface claim, detach handling ([28334dc](https://github.com/zackpollard/fuji-cull/commit/28334dcecdac35f3a5ceb8942c8c3381c1da86bd))


### Bug Fixes

* swallow the ghost TEXTINPUT from the keypress that opens the import panel ([#11](https://github.com/zackpollard/fuji-cull/issues/11)) ([86702c5](https://github.com/zackpollard/fuji-cull/commit/86702c5482c01de506ce866e5d99fef164cc8f0f))

## [1.3.0](https://github.com/zackpollard/fuji-cull/compare/v1.2.2...v1.3.0) (2026-07-12)


### Features

* aft patch gains --device-fd for android usb host api ([56150aa](https://github.com/zackpollard/fuji-cull/commit/56150aaffbc5fe3bd0cb055f56ee007550987871))
* android app scaffold — kotlin/compose culling mvp ([bf6401d](https://github.com/zackpollard/fuji-cull/commit/bf6401d61102bca114801f00d8d89a982d2b9f13))
* android groundwork — usb fd plumbing and gomobile facade ([c881ed9](https://github.com/zackpollard/fuji-cull/commit/c881ed9db366c0432bf5715bce600b1a47ddc61d))
* badge shots already uploaded to immich ([93a1b5d](https://github.com/zackpollard/fuji-cull/commit/93a1b5dddb5eb295d8e622a23ec22ca9ae80d814))
* pure-go exif restamping and raf preview extraction ([2f23e47](https://github.com/zackpollard/fuji-cull/commit/2f23e47111cadb7b61698581d6b726006bfb59f0))


### Bug Fixes

* android crash on launch — defer connected-device fgs until usb grant ([7479749](https://github.com/zackpollard/fuji-cull/commit/74797495cf3c227a1d5e7912c89a5ae16b90c942))
* android startup — honor FUJI_AFT in ensure, surface engine errors on screen ([8d5093c](https://github.com/zackpollard/fuji-cull/commit/8d5093c01b183b7ac54cd0b34cb95e6963bce722))
* gomobile getters are methods in kotlin ([eb02f3e](https://github.com/zackpollard/fuji-cull/commit/eb02f3e58c2d0071a64ba812dc8003212f3a4f28))

## [1.2.2](https://github.com/zackpollard/fuji-cull/compare/v1.2.1...v1.2.2) (2026-07-10)


### Bug Fixes

* macos camera daemon contention and false healing accumulation ([be61cc3](https://github.com/zackpollard/fuji-cull/commit/be61cc3555c65d6d37ac206dd9839d5e2fe52293))

## [1.2.1](https://github.com/zackpollard/fuji-cull/compare/v1.2.0...v1.2.1) (2026-07-09)


### Bug Fixes

* bundle sdl3 for sdl2-compat dlopen in macos app ([fd2d001](https://github.com/zackpollard/fuji-cull/commit/fd2d001833f8721e3c8b4d04b606049a85ee2595))
* dereference sdl3 symlink when bundling ([341e25d](https://github.com/zackpollard/fuji-cull/commit/341e25d7f716e0bac6c6ae4b90df2ac2a6138e97))
* render in physical pixels (macos retina), ctrl +/- ui zoom ([f9b2cfc](https://github.com/zackpollard/fuji-cull/commit/f9b2cfc53ac70f915e7909e4cf8e7393ec5fc042))
* ship sdl3 under the unversioned name sdl2-compat dlopens ([f0b19a5](https://github.com/zackpollard/fuji-cull/commit/f0b19a5b25fcc283d0c4844392909c4970f4977d))

## [1.2.0](https://github.com/zackpollard/fuji-cull/compare/v1.1.0...v1.2.0) (2026-07-09)


### Features

* head sweep is the primary thumbnail path ([2da444a](https://github.com/zackpollard/fuji-cull/commit/2da444ac9fbb3e4a43830e0d3c8f05893e016c30))
* macos app bundle dmg ([#5](https://github.com/zackpollard/fuji-cull/issues/5)) ([344e5b2](https://github.com/zackpollard/fuji-cull/commit/344e5b2750b99e3f27ad005d9831dc03485f0c79))
* remember import destination and album across sessions ([eafae21](https://github.com/zackpollard/fuji-cull/commit/eafae2117cecd8e3a6c723784dae3399b5416396))
* stack RAF+JPG pairs in Immich after upload ([#7](https://github.com/zackpollard/fuji-cull/issues/7)) ([7b0ad61](https://github.com/zackpollard/fuji-cull/commit/7b0ad612ae77fb22679809b193c775ec0c0a7df5))


### Bug Fixes

* tighten head/poster batch timeouts so usb wedges cost seconds ([82182b5](https://github.com/zackpollard/fuji-cull/commit/82182b51318ff05bde451d9849fe2c4665eaa65c))


### Performance Improvements

* batch video poster head pulls, parallel ffmpeg extraction ([454a087](https://github.com/zackpollard/fuji-cull/commit/454a0876583763f7d70a96ad555f69ff341ea485))

## [1.1.0](https://github.com/zackpollard/fuji-cull/compare/v1.0.0...v1.1.0) (2026-07-09)


### Features

* macos builds ([#4](https://github.com/zackpollard/fuji-cull/issues/4)) ([28423ad](https://github.com/zackpollard/fuji-cull/commit/28423ad0ab8d28416b5a729c4471bcdea315dc8c))


### Bug Fixes

* adaptive frame cap - throttle only vsync-less spin frames ([87841e7](https://github.com/zackpollard/fuji-cull/commit/87841e75df21db3b957ea0931efe5f6efeaf76ca))
* cap frame rate (wayland vsync does not block when unfocused) ([d8d7fee](https://github.com/zackpollard/fuji-cull/commit/d8d7feeea4c7a2a0baa83bf60aef904af70f7904))
* park render loop in waiteventtimeout when vsync is not blocking ([deb5b69](https://github.com/zackpollard/fuji-cull/commit/deb5b69024ef1babccbfa7a48958ed6beb47c177))

## 1.0.0 (2026-07-09)


### Features

* add fuji-cull web ui for culling photos straight off the camera ([be6cbc7](https://github.com/zackpollard/fuji-cull/commit/be6cbc7c1ad86701715062dd3c31db9372a4bbeb))
* add native sdl frontend with libjpeg-turbo multicore decode ([90af28d](https://github.com/zackpollard/fuji-cull/commit/90af28d1650fa19398dbce46b2ff734e70697a31))
* camera stale-buffer breaker indicator in both headers ([bef3fa1](https://github.com/zackpollard/fuji-cull/commit/bef3fa12a479b956c940d657148e11b435e2710d))
* exif orientation store with rotated thumbnail delivery ([7e5603c](https://github.com/zackpollard/fuji-cull/commit/7e5603c81a626829b21623d7131843017f3aa42a))
* generate missing thumbnails locally from buffered full images ([559e3ff](https://github.com/zackpollard/fuji-cull/commit/559e3ffccc778e1a5439ea811bc09b2d14aa7e42))
* gui feature parity - embedded mpv video, grid view, import panel ([7ba1c00](https://github.com/zackpollard/fuji-cull/commit/7ba1c003340bb46cf969f5e37476823e2b8fb319))
* heal camera-impossible thumbs from exif-embedded previews ([06d0b77](https://github.com/zackpollard/fuji-cull/commit/06d0b7769e1c9f84ce8f0fb692dd7889ce3d971c))
* large preemptible image batches ([2aa621a](https://github.com/zackpollard/fuji-cull/commit/2aa621a3c702f9de0447ebf53f7974a75800d327))
* larger forward-biased decode buffer, tunable via flags ([d0f747c](https://github.com/zackpollard/fuji-cull/commit/d0f747cc69d3d4eebc100ddc18e041c9f03876ff))
* log video decode path (hwdec vs software fallback) ([bbdc2df](https://github.com/zackpollard/fuji-cull/commit/bbdc2df23241d3b20a306712c404860379bdcb8c))
* metadata coverage counter in both headers ([cbeb2f1](https://github.com/zackpollard/fuji-cull/commit/cbeb2f1b8a8d5b1a7ae303951f6a807069454300))
* partial-read breaker self-probes every 3 minutes ([25da6d4](https://github.com/zackpollard/fuji-cull/commit/25da6d4b849dec35b207e9acad4bd1a0c0d1c5f9))
* persist camera-impossible thumbnail set across restarts ([1c8c802](https://github.com/zackpollard/fuji-cull/commit/1c8c80237fb48aa9508d1f9a7fa9b788c06d4855))
* pipelined thumb healing and explicit healing counter in both uis ([1a3d8d4](https://github.com/zackpollard/fuji-cull/commit/1a3d8d4158cdd2ab0f1f3c32e2942d3929bebe31))
* port fuji-import into fuji-tools module with shared packages ([1dd99fc](https://github.com/zackpollard/fuji-cull/commit/1dd99fc1237471a545201714942c570a8983a73b))
* preload grid thumbnails beyond the fold ([116a2c9](https://github.com/zackpollard/fuji-cull/commit/116a2c9d27eae5fc62ccca30bb9a4aa4faec5150))
* retry discovery until a camera appears ([ed0b574](https://github.com/zackpollard/fuji-cull/commit/ed0b5749d57629f9648e7a7c83e38ac0ead53241))
* self-contained appimage bundle ([#2](https://github.com/zackpollard/fuji-cull/issues/2)) ([30344f2](https://github.com/zackpollard/fuji-cull/commit/30344f2b0509f94e96dc1a920e3cef3f91962f2e))
* stream videos straight off the camera ([c955e69](https://github.com/zackpollard/fuji-cull/commit/c955e6977eaef15ae257efa22cff8821e9915e77))
* video poster thumbnails via mtp partial reads (patched aft-mtp-cli) ([599a0d7](https://github.com/zackpollard/fuji-cull/commit/599a0d74b64a3d3806b737fd1db228cd0dc2fb93))
* wasd one-handed culling cluster ([b3c9b8f](https://github.com/zackpollard/fuji-cull/commit/b3c9b8f0abf5975e94bc88ff61cc96561c71952b))
* zero-copy gl video rendering ([422748b](https://github.com/zackpollard/fuji-cull/commit/422748bb7fe3fd422694329be2aa68540139d96f))


### Bug Fixes

* cheap sick-camera probes, auto-reenable partial reads on recovery ([96d8073](https://github.com/zackpollard/fuji-cull/commit/96d80732cff444f0bb7edddcfc46dd237bd1fbe7))
* chunked, validated, retried import pulls ([6d3bffb](https://github.com/zackpollard/fuji-cull/commit/6d3bffbde4f524acc8e1d22087ded805c4baee98))
* detect fragment thumbnails (soi check), heal via full-image pulls + local generation ([705896d](https://github.com/zackpollard/fuji-cull/commit/705896de093d69ec4c1caee0099ebc5298d7d1ae))
* exif orientation normalization in turbo decode ([595ebca](https://github.com/zackpollard/fuji-cull/commit/595ebca972b2c0d86843799f49c9fc34e93083d8))
* grid scroll fighting cursor snap and thumb draw budget ([3306f11](https://github.com/zackpollard/fuji-cull/commit/3306f1106e1a20ed2b1e43dfbd05f03826373ac6))
* gui texture thrash strobe and missing disk-buffer indicators ([e688b07](https://github.com/zackpollard/fuji-cull/commit/e688b073fb95c7105e288f359dd4e4ff5e10ebd4))
* import fails fast on all-garbage chunk, trips camera breaker ([42535a9](https://github.com/zackpollard/fuji-cull/commit/42535a9eca0d601f42e9b4f48cd9c43cb0358d8a))
* keep gl context handle as unsafe.Pointer (go vet) ([c9b982a](https://github.com/zackpollard/fuji-cull/commit/c9b982ac0294c41b835f696b6b43b4b4c72ff955))
* magic-check bulk transfers, purge and breaker for stale-buffer garbage ([cb2f4a4](https://github.com/zackpollard/fuji-cull/commit/cb2f4a442e696e992011b9ae0ac9aab64907868f))
* modern immich api routes (v1.106+/v2/v3 plural endpoints) ([b87b686](https://github.com/zackpollard/fuji-cull/commit/b87b68646ec716c361244f7913f9ea84600ccb7b))
* mpv hwdec copy-back and fast sw scaling for smooth embedded video ([d16cda6](https://github.com/zackpollard/fuji-cull/commit/d16cda6f0c1728eacbde25fb7a2305249f8d4d3b))
* pass native display for zero-copy interop, correct fbo flip ([a7bc9c5](https://github.com/zackpollard/fuji-cull/commit/a7bc9c56c64436c6db6c6a3d7cd5903f10b08ce9))
* prefer native wayland sdl driver for egl (zero-copy needs it) ([b6ef89c](https://github.com/zackpollard/fuji-cull/commit/b6ef89cfd99a1d4bba759c8319803a85d493579d))
* reprobe sick camera every 20s instead of 3 minutes ([942b183](https://github.com/zackpollard/fuji-cull/commit/942b1836d31304cb9c53a30e6ce432920581bffb))
* suspend photo texture uploads during video playback ([4fb3a56](https://github.com/zackpollard/fuji-cull/commit/4fb3a56162d24f2463fe3d0a45ff051ec480f3a4))
* thumb texture cache larger than a fullscreen 4k grid ([f57da44](https://github.com/zackpollard/fuji-cull/commit/f57da449e2b4307710c4e5c0a82915be7c4e1370))
* timeout on image fetches - wedged usb transfer froze the pipeline ([292a8d5](https://github.com/zackpollard/fuji-cull/commit/292a8d54c68464eae07e0328acae659a74d07b90))
* validate jpeg completeness before banking thumbnails, purge truncated cache ([983b0af](https://github.com/zackpollard/fuji-cull/commit/983b0af9df9b7ceb18970b0f6436d86e7b8ae640))


### Performance Improvements

* 256-shot orientation batches to amortize session setup ([a6984b8](https://github.com/zackpollard/fuji-cull/commit/a6984b89725416aebf83cc4a6c2cbef2650f7306))
