# Changelog

## [0.6.0](https://github.com/s3ntin3l8/branchdam/compare/v0.5.0...v0.6.0) (2026-08-22)


### Features

* **graph:** link DJI .srt telemetry sidecars via content-sniffing ([#251](https://github.com/s3ntin3l8/branchdam/issues/251)) ([08c2859](https://github.com/s3ntin3l8/branchdam/commit/08c2859636f684b5a8eb7c7b5434193491ed1b10))
* **projectfile:** register .xmp sidecars and resolve same-stem siblings ([#249](https://github.com/s3ntin3l8/branchdam/issues/249)) ([aebf10a](https://github.com/s3ntin3l8/branchdam/commit/aebf10a654cea6dcffd527bcfc7543d1ab796662))


### Bug Fixes

* **pipeline:** drop storage_locations.name UNIQUE to allow rootPath edits ([#253](https://github.com/s3ntin3l8/branchdam/issues/253)) ([e554310](https://github.com/s3ntin3l8/branchdam/commit/e554310d72c1f00a4b8af9a443bbe55d08cbc76e))
* **prune:** re-verify Tier-3 ancestor file on disk before delete ([#246](https://github.com/s3ntin3l8/branchdam/issues/246)) ([0c3c78d](https://github.com/s3ntin3l8/branchdam/commit/0c3c78d566b645e9e9b8f99467774e4ab5401918))

## [0.5.0](https://github.com/s3ntin3l8/branchdam/compare/v0.4.0...v0.5.0) (2026-08-21)


### Features

* **agent,pipeline:** DJI .SRT single-point geotagging, plus GPS on the agent-ingest path ([#250](https://github.com/s3ntin3l8/branchdam/issues/250)) ([2967f5f](https://github.com/s3ntin3l8/branchdam/commit/2967f5f3edcd65da5485345c3be8e8dcd44d8c96))
* **pipeline,httpapi:** differential re-index option for Tier-3 locations ([#254](https://github.com/s3ntin3l8/branchdam/issues/254)) ([09d8384](https://github.com/s3ntin3l8/branchdam/commit/09d8384429348737885fb192b7b061d91e3d80bd))
* **probe:** merge sidecar .xmp metadata into RAW probe results ([#247](https://github.com/s3ntin3l8/branchdam/issues/247)) ([b9108a2](https://github.com/s3ntin3l8/branchdam/commit/b9108a2e0c913e5231628367f19ef14c6c7fc730))
* **thumbs:** video poster thumbnails via ffmpeg ([#248](https://github.com/s3ntin3l8/branchdam/issues/248)) ([abdbc29](https://github.com/s3ntin3l8/branchdam/commit/abdbc2900d0a9898a26626e11916f0f7e566c238))


### Bug Fixes

* **graph,pipeline:** DJI-style .LRF proxies link with arbitrary direction and a meaningless label ([#244](https://github.com/s3ntin3l8/branchdam/issues/244)) ([e3abb92](https://github.com/s3ntin3l8/branchdam/commit/e3abb92f068e4df49093034603f2488b2eb3e03a))
* **pipeline,httpapi:** cacheTtlHours/watch/sweep silently orphan on a rootPath edit ([#252](https://github.com/s3ntin3l8/branchdam/issues/252)) ([779278a](https://github.com/s3ntin3l8/branchdam/commit/779278a61bdbeede05083b8b434c39c092eb0d34))
* **pipeline:** empty Tier-3 mount marks the entire archive MISSING ([#245](https://github.com/s3ntin3l8/branchdam/issues/245)) ([32b0514](https://github.com/s3ntin3l8/branchdam/commit/32b05145f9a4df6c286fc27664a0659a6acf9939))
* **thumbs,httpapi:** restoring branchdam.db without /data/thumbs leaves every node stuck READY ([#241](https://github.com/s3ntin3l8/branchdam/issues/241)) ([f1a5585](https://github.com/s3ntin3l8/branchdam/commit/f1a5585765329c2e199a4cdeb0390c0f6e261805))
* **thumbs:** ListPendingThumbnails has no tier filter ([#243](https://github.com/s3ntin3l8/branchdam/issues/243)) ([eea3909](https://github.com/s3ntin3l8/branchdam/commit/eea3909e851b770bc8e6af35c70a7e6a005ea833))

## [0.4.0](https://github.com/s3ntin3l8/branchdam/compare/v0.3.1...v0.4.0) (2026-08-21)


### Features

* **httpapi,web:** thumbnail HTTP route and SPA rendering (T4) ([#223](https://github.com/s3ntin3l8/branchdam/issues/223)) ([98a05b8](https://github.com/s3ntin3l8/branchdam/commit/98a05b803658b6c91e26101f746d588dba2f81af))
* **thumbs,config:** add the JPEG thumbnail cache package and its config ([#220](https://github.com/s3ntin3l8/branchdam/issues/220)) ([fe02eb5](https://github.com/s3ntin3l8/branchdam/commit/fe02eb53ee80cc6f74cc7a19798d53cdabfe125f))
* **thumbs,db,httpapi:** thumbnail state tracking and background worker (T3) ([#222](https://github.com/s3ntin3l8/branchdam/issues/222)) ([0a14e98](https://github.com/s3ntin3l8/branchdam/commit/0a14e98258efc9db1b772168b98f4455ce39ad9e))

## [0.3.1](https://github.com/s3ntin3l8/branchdam/compare/v0.3.0...v0.3.1) (2026-08-21)


### Bug Fixes

* **graph:** gate index-suffix filename_stem matches below auto-accept ([#214](https://github.com/s3ntin3l8/branchdam/issues/214)) ([847329f](https://github.com/s3ntin3l8/branchdam/commit/847329f2ae818c3806639819ec3cf403aa582d36))
* **httpapi:** handleCreateEdge doesn't refuse a manual edge targeting a project-file node ([#210](https://github.com/s3ntin3l8/branchdam/issues/210)) ([26f48fc](https://github.com/s3ntin3l8/branchdam/commit/26f48fcad3009ebe004fcb8f06ca2258ed77362b))
* **httpapi:** sanitize request path before logging (CodeQL go/log-injection) ([#215](https://github.com/s3ntin3l8/branchdam/issues/215)) ([036e89d](https://github.com/s3ntin3l8/branchdam/commit/036e89d278cc31d518a753e2da920161a1ac1ed5))
* **pipeline,graph:** re-promote captured_at_unix on touch/rebase ([#204](https://github.com/s3ntin3l8/branchdam/issues/204)) ([#217](https://github.com/s3ntin3l8/branchdam/issues/217)) ([1809c16](https://github.com/s3ntin3l8/branchdam/commit/1809c16b2a686468358101328b85b41eb66e7204))
* **web:** SSE progress nudges don't refresh a mounted asset detail/graph/lineage view ([#212](https://github.com/s3ntin3l8/branchdam/issues/212)) ([5139009](https://github.com/s3ntin3l8/branchdam/commit/51390094f99125f2cc33c44e6f7b3d726f844cbb))


### Performance Improvements

* **httpapi:** parallelize per-location statfs probes in handleStorageHealth ([#213](https://github.com/s3ntin3l8/branchdam/issues/213)) ([949125b](https://github.com/s3ntin3l8/branchdam/commit/949125be7244d20efe4c3342860c5811363b1639))

## [0.3.0](https://github.com/s3ntin3l8/branchdam/compare/v0.2.0...v0.3.0) (2026-08-20)


### Features

* **agent-api:** complete agent-server contract, event drainer, and protocol ADR ([#27](https://github.com/s3ntin3l8/branchdam/issues/27), [#57](https://github.com/s3ntin3l8/branchdam/issues/57), [#58](https://github.com/s3ntin3l8/branchdam/issues/58), [#59](https://github.com/s3ntin3l8/branchdam/issues/59)) ([#136](https://github.com/s3ntin3l8/branchdam/issues/136)) ([0b479fa](https://github.com/s3ntin3l8/branchdam/commit/0b479fadc4f96ba0d7fcdab5b0238f1f844acf40))
* **agent:** allow LOCAL_STAGING -&gt; CENTRAL_TIER3 rebase when the file already exists ([#178](https://github.com/s3ntin3l8/branchdam/issues/178)) ([b795ff7](https://github.com/s3ntin3l8/branchdam/commit/b795ff76c70f696c500a0443654d64bc3e56e2be))
* **agent:** persist event_queue retry_count and add backoff on empty drain passes ([#147](https://github.com/s3ntin3l8/branchdam/issues/147)) ([#152](https://github.com/s3ntin3l8/branchdam/issues/152)) ([a0f35d6](https://github.com/s3ntin3l8/branchdam/commit/a0f35d6946ad7abdf830091c519a557955f0f064))
* **agent:** wire the event_queue drainer into cmd/branchdam/main.go ([#189](https://github.com/s3ntin3l8/branchdam/issues/189)) ([7143479](https://github.com/s3ntin3l8/branchdam/commit/7143479aa4fbf15646ba82c74311564d47829e45))
* **api,web:** operator path rewrites REST API & UI configuration ([#101](https://github.com/s3ntin3l8/branchdam/issues/101)) ([#110](https://github.com/s3ntin3l8/branchdam/issues/110)) ([90a34bb](https://github.com/s3ntin3l8/branchdam/commit/90a34bb37e51fd79d1fc757c772ff0bcb0096b89))
* **api,web:** storage health — tier capacity and queue gauges ([#51](https://github.com/s3ntin3l8/branchdam/issues/51)) ([#117](https://github.com/s3ntin3l8/branchdam/issues/117)) ([3229733](https://github.com/s3ntin3l8/branchdam/commit/32297331e5af9bd94d6bcdbcf44cb618ade7e54c))
* **api,web:** sync status + manual PUSH_FAILED re-trigger ([#156](https://github.com/s3ntin3l8/branchdam/issues/156)) ([#170](https://github.com/s3ntin3l8/branchdam/issues/170)) ([40ec517](https://github.com/s3ntin3l8/branchdam/commit/40ec517399ca0fb1964b01e5a2e2e751392fafed))
* **api:** bounded multi-hop lineage traversal ([#48](https://github.com/s3ntin3l8/branchdam/issues/48)) ([#113](https://github.com/s3ntin3l8/branchdam/issues/113)) ([4699c13](https://github.com/s3ntin3l8/branchdam/commit/4699c13fface8fa04d6aaeb79034d9e1831f0175))
* **api:** GET /api/v1/storage-locations for the ingest picker ([#35](https://github.com/s3ntin3l8/branchdam/issues/35)) ([08abf49](https://github.com/s3ntin3l8/branchdam/commit/08abf49004f1ac47de790a72d33b423eb82bf4e2))
* **auth:** group-based authorization middleware ([#37](https://github.com/s3ntin3l8/branchdam/issues/37)) ([099b77b](https://github.com/s3ntin3l8/branchdam/commit/099b77b3599267f981cacb5177803c002b5a90aa))
* **auth:** group-based authorization middleware ([#37](https://github.com/s3ntin3l8/branchdam/issues/37)) ([c894366](https://github.com/s3ntin3l8/branchdam/commit/c8943665afa7eb2917611872fc4ad1b866714d5e))
* **config:** per-location watch opt-in for continuous ingest ([#32](https://github.com/s3ntin3l8/branchdam/issues/32)) ([0944a87](https://github.com/s3ntin3l8/branchdam/commit/0944a877cddf49d5a5dc27efb89e29f6bcb16ced))
* **db,sync:** bound remote_sync_state retries with a retry_count column ([#182](https://github.com/s3ntin3l8/branchdam/issues/182)) ([#200](https://github.com/s3ntin3l8/branchdam/issues/200)) ([016dc2f](https://github.com/s3ntin3l8/branchdam/commit/016dc2f9795a8e4b76884d0abc5bb6f5ab9911a2))
* **db,sync:** remote_sync_state queries and the push state machine ([#53](https://github.com/s3ntin3l8/branchdam/issues/53)) ([#126](https://github.com/s3ntin3l8/branchdam/issues/126)) ([989c3bd](https://github.com/s3ntin3l8/branchdam/commit/989c3bdd2c31e027b60fcf736b462d352d05d64a))
* **db:** add MarkUnseenNodesMissing set-based sweep query (phase 1 [#31](https://github.com/s3ntin3l8/branchdam/issues/31)) ([72b30c4](https://github.com/s3ntin3l8/branchdam/commit/72b30c481575a9e544099203c5bb18dc0ddc1180))
* **db:** migration 00002 — promote camera_serial/lens_model, index captured_at_unix ([#39](https://github.com/s3ntin3l8/branchdam/issues/39)) ([#78](https://github.com/s3ntin3l8/branchdam/issues/78)) ([7adbc89](https://github.com/s3ntin3l8/branchdam/commit/7adbc89e693380d592d120069e3754937f634fce))
* **db:** node_metadata insert/list queries for EXIF overflow ([#33](https://github.com/s3ntin3l8/branchdam/issues/33)) ([dbf1876](https://github.com/s3ntin3l8/branchdam/commit/dbf18769d8cce975d23f0222bd987ea59254c9c9))
* **db:** prune node_metadata for archived nodes ([#89](https://github.com/s3ntin3l8/branchdam/issues/89)) ([#129](https://github.com/s3ntin3l8/branchdam/issues/129)) ([be8a85b](https://github.com/s3ntin3l8/branchdam/commit/be8a85bb9881cf2c449961ad208963b108b57bf2))
* **graph:** .dam.json manifest ingestion → Tier-1 edges at confidence 1.00 ([#45](https://github.com/s3ntin3l8/branchdam/issues/45)) ([#84](https://github.com/s3ntin3l8/branchdam/issues/84)) ([0996a47](https://github.com/s3ntin3l8/branchdam/commit/0996a479d49276795783fced951de44cb22e2475))
* **graph:** .drp archive introspection (unzip in memory, parse project.xml) ([#46](https://github.com/s3ntin3l8/branchdam/issues/46)) ([#94](https://github.com/s3ntin3l8/branchdam/issues/94)) ([a030f97](https://github.com/s3ntin3l8/branchdam/commit/a030f97cc317c5db81f5004527e6d1de8d1c1232))
* **graph:** .fcpxml and .edl parsers ([#47](https://github.com/s3ntin3l8/branchdam/issues/47)) ([#95](https://github.com/s3ntin3l8/branchdam/issues/95)) ([f263eec](https://github.com/s3ntin3l8/branchdam/commit/f263eecee76e0976be7fb6f099ccd9a3b3507798))
* **graph:** Adobe Premiere Pro (.prproj) project file parser ([#100](https://github.com/s3ntin3l8/branchdam/issues/100)) ([#109](https://github.com/s3ntin3l8/branchdam/issues/109)) ([dcc67e0](https://github.com/s3ntin3l8/branchdam/commit/dcc67e0d241fc16d9cd8459ff813e5ec6f377afa))
* **graph:** extend Node/Lookup for spatial-temporal candidate lookup ([#42](https://github.com/s3ntin3l8/branchdam/issues/42)) ([#81](https://github.com/s3ntin3l8/branchdam/issues/81)) ([a346bad](https://github.com/s3ntin3l8/branchdam/commit/a346badf3870ef109c2bc4c4df266e4802eec888))
* **graph:** Tier-3 heuristic resolver (serial + lens + ±2s + Hamming &lt;= 10) ([#43](https://github.com/s3ntin3l8/branchdam/issues/43)) ([#82](https://github.com/s3ntin3l8/branchdam/issues/82)) ([b1b5540](https://github.com/s3ntin3l8/branchdam/commit/b1b5540bb0a45bdd43ab51faea68f56b48cf36fa))
* **httpapi,pipeline:** reject a duplicate concurrent FULL_SCAN for the same storage location ([#179](https://github.com/s3ntin3l8/branchdam/issues/179)) ([3037ee0](https://github.com/s3ntin3l8/branchdam/commit/3037ee032f47b835f3c07a7b0cf524a49a953152))
* **httpapi,sync:** surface retry_count and log when a row permanently exhausts its retry bound ([#207](https://github.com/s3ntin3l8/branchdam/issues/207)) ([a64101c](https://github.com/s3ntin3l8/branchdam/commit/a64101c58cece06fb26b25a1070e046a324fb6d2))
* **httpapi:** gate mutating routes and the OpenAPI surface on role ([#38](https://github.com/s3ntin3l8/branchdam/issues/38)) ([3e04929](https://github.com/s3ntin3l8/branchdam/commit/3e04929e57b9d5c99e96c3d0a01699bb551ec6e8))
* **httpapi:** gate mutating routes and the OpenAPI surface on role ([#38](https://github.com/s3ntin3l8/branchdam/issues/38)) ([5d41236](https://github.com/s3ntin3l8/branchdam/commit/5d41236cbda08225151e5f3d005efa756861317c))
* **immich:** external-library scan trigger and sync tracking ([#55](https://github.com/s3ntin3l8/branchdam/issues/55)) ([#145](https://github.com/s3ntin3l8/branchdam/issues/145)) ([de2df09](https://github.com/s3ntin3l8/branchdam/commit/de2df095c40a19671abb4438d8541959909bf55b))
* **indexer,pipeline:** low-priority differential mtime sweeper for SMB/NFS ([#161](https://github.com/s3ntin3l8/branchdam/issues/161)) ([8d1b8ea](https://github.com/s3ntin3l8/branchdam/commit/8d1b8ea31caf213425025f57cfaba6becfbc04f8))
* **indexer:** surface file removals via Watch's onRemove callback ([#32](https://github.com/s3ntin3l8/branchdam/issues/32)) ([e359f71](https://github.com/s3ntin3l8/branchdam/commit/e359f715ab3fb80ea6a70cbb8a156cdfbd9f941a))
* **metadata:** EXIF/XMP inheritance from parent to child ([#54](https://github.com/s3ntin3l8/branchdam/issues/54)) ([#135](https://github.com/s3ntin3l8/branchdam/issues/135)) ([1de2d9d](https://github.com/s3ntin3l8/branchdam/commit/1de2d9d39d6e035a22ad56db696117d346011b8d))
* **mullion,make:** add Mullion dock/actions config and dev Make targets ([#209](https://github.com/s3ntin3l8/branchdam/issues/209)) ([ac95a3f](https://github.com/s3ntin3l8/branchdam/commit/ac95a3f33bc3ae77c6a358568883350d7bd752ab))
* **pipeline,api:** backfill child node_metadata after EXIF/XMP inheritance ([#157](https://github.com/s3ntin3l8/branchdam/issues/157)) ([#172](https://github.com/s3ntin3l8/branchdam/issues/172)) ([aa47825](https://github.com/s3ntin3l8/branchdam/commit/aa4782550c178c49a5270ec22dc6f22ecaff80d2))
* **pipeline:** compute and persist phash, including RAW via embedded preview ([#40](https://github.com/s3ntin3l8/branchdam/issues/40)) ([#79](https://github.com/s3ntin3l8/branchdam/issues/79)) ([20613a4](https://github.com/s3ntin3l8/branchdam/commit/20613a45e620cce0ca420ceb62e07b750840ca72))
* **pipeline:** persist EXIF metadata into node_metadata inside Commit ([#33](https://github.com/s3ntin3l8/branchdam/issues/33)) ([a78712f](https://github.com/s3ntin3l8/branchdam/commit/a78712fc021f2e03523ea6c476f8477fde04bc57))
* **pipeline:** persist ffprobe structured metadata inside Commit ([#34](https://github.com/s3ntin3l8/branchdam/issues/34)) ([4f4d29f](https://github.com/s3ntin3l8/branchdam/commit/4f4d29f2bc6dcc55644913af0e77e86fe779c974))
* **pipeline:** persist ffprobe video-stream metadata ([#34](https://github.com/s3ntin3l8/branchdam/issues/34)) ([73776e4](https://github.com/s3ntin3l8/branchdam/commit/73776e48b1aaffc828d6c2ad99fc1ecf09cef17e))
* **pipeline:** persist full EXIF payload into node_metadata ([#33](https://github.com/s3ntin3l8/branchdam/issues/33)) ([1fabac9](https://github.com/s3ntin3l8/branchdam/commit/1fabac9937d865cd8df5640b5e66a9dea58345bf))
* **pipeline:** processFile populates the full typed EXIF payload ([#33](https://github.com/s3ntin3l8/branchdam/issues/33)) ([b72158e](https://github.com/s3ntin3l8/branchdam/commit/b72158e65080a1a062c8bbf73fe02bd60ef7acec))
* **pipeline:** processFile runs FFProbe under the bounded exifTimeout ([#34](https://github.com/s3ntin3l8/branchdam/issues/34)) ([85e47d9](https://github.com/s3ntin3l8/branchdam/commit/85e47d944e31e3f0daacceb294c0481dc31fb724))
* **pipeline:** reconcile orphaned RUNNING scan_jobs rows at startup ([c5f4b0a](https://github.com/s3ntin3l8/branchdam/commit/c5f4b0a0d6a92f885918d2896482462a514ddbe5))
* **pipeline:** record a shutdown-interrupted scan as CANCELLED, not COMPLETED ([22d6342](https://github.com/s3ntin3l8/branchdam/commit/22d6342bc203ba6ec8d7a65c81d9fd06ad3ae01e))
* **pipeline:** Result gains FFProbe field + closed video-extension gate ([#34](https://github.com/s3ntin3l8/branchdam/issues/34)) ([fcb52fe](https://github.com/s3ntin3l8/branchdam/commit/fcb52fe8af4420d2d4618002ad5e7606efef1a3e))
* **pipeline:** Result gains typed EXIF fields + allowlisted Raw subset ([#33](https://github.com/s3ntin3l8/branchdam/issues/33)) ([3c00b92](https://github.com/s3ntin3l8/branchdam/commit/3c00b922395f1830004b2719e780f16ab1c8b70d))
* **pipeline:** watch-driven continuous ingest ([#32](https://github.com/s3ntin3l8/branchdam/issues/32)) ([1244f18](https://github.com/s3ntin3l8/branchdam/commit/1244f181c44b68a9eb5e1730ec0b18cf3b2e20aa))
* **pipeline:** WatcherSupervisor running indexer.Watch per location ([#32](https://github.com/s3ntin3l8/branchdam/issues/32)) ([ce2d75e](https://github.com/s3ntin3l8/branchdam/commit/ce2d75e4626deaddec7fd1dd0c96c87d0f47a631))
* **pruning:** TTL cache pruning engine gated on verified Tier-3 full_hash ([#177](https://github.com/s3ntin3l8/branchdam/issues/177)) ([17f7a73](https://github.com/s3ntin3l8/branchdam/commit/17f7a73c5c6676bc6a65f5c3551df467d9656e9a))
* **web:** audit queue side-by-side diff and manual link ([#50](https://github.com/s3ntin3l8/branchdam/issues/50)) ([#115](https://github.com/s3ntin3l8/branchdam/issues/115)) ([3d5fc6d](https://github.com/s3ntin3l8/branchdam/commit/3d5fc6d4a2118cf02b4b0f2c7f6c9dac9bfd2b20))
* **web:** ingest jobs page, camera filters, unlinked-only, pagination ([#52](https://github.com/s3ntin3l8/branchdam/issues/52)) ([#118](https://github.com/s3ntin3l8/branchdam/issues/118)) ([970f481](https://github.com/s3ntin3l8/branchdam/commit/970f48182c4f6ecf37b908bc1da4f8ed8abbf0b1))
* **web:** IngestPage scan trigger + live progress, wired into nav and router ([#35](https://github.com/s3ntin3l8/branchdam/issues/35)) ([718cdc6](https://github.com/s3ntin3l8/branchdam/commit/718cdc6011555482419551eaeddea0322e32468f))
* **web:** multi-hop canvas, spec node colours, unlinked badge ([#49](https://github.com/s3ntin3l8/branchdam/issues/49)) ([#114](https://github.com/s3ntin3l8/branchdam/issues/114)) ([0527be3](https://github.com/s3ntin3l8/branchdam/commit/0527be3247b7f7ada3713a685260ab99449efba0))
* **web:** root ErrorBoundary and Vitest ReactFlow warning suppression ([#121](https://github.com/s3ntin3l8/branchdam/issues/121)) ([974396d](https://github.com/s3ntin3l8/branchdam/commit/974396d3a8201317592540ed55ee4897f7c8cbfc))
* **web:** scan trigger and live progress UI ([#35](https://github.com/s3ntin3l8/branchdam/issues/35)) ([20625b0](https://github.com/s3ntin3l8/branchdam/commit/20625b0ae2e3af15235ea03aef0b6f9cc2324fdc))
* **web:** storage-locations client + useStorageLocations hook ([#35](https://github.com/s3ntin3l8/branchdam/issues/35)) ([5dd9bcd](https://github.com/s3ntin3l8/branchdam/commit/5dd9bcd97eacac88b137f9568b0420238e93ef88))
* **web:** wire the inherit-metadata action into AssetDetailPage ([#186](https://github.com/s3ntin3l8/branchdam/issues/186)) ([#206](https://github.com/s3ntin3l8/branchdam/issues/206)) ([75c52f8](https://github.com/s3ntin3l8/branchdam/commit/75c52f89fcb13af2802040101373684d17a2370e))


### Bug Fixes

* **agent:** drainer and rebase correctness before the worker ships ([#171](https://github.com/s3ntin3l8/branchdam/issues/171)) ([e7780b2](https://github.com/s3ntin3l8/branchdam/commit/e7780b265f3226a777c9d59a0d96bb3e11393081))
* **api:** deterministic pickWinningParent tie-break ([#160](https://github.com/s3ntin3l8/branchdam/issues/160)) ([#175](https://github.com/s3ntin3l8/branchdam/issues/175)) ([2ede759](https://github.com/s3ntin3l8/branchdam/commit/2ede75971ff134ccb0b3dd78ac9bf4a1b9948de0))
* **auth:** fail closed on a mutating request with no identity headers at all ([#190](https://github.com/s3ntin3l8/branchdam/issues/190)) ([ab0c1fe](https://github.com/s3ntin3l8/branchdam/commit/ab0c1fe511dec64a138185f0f45094244d5c98c3))
* **ci:** gate hermes auto-review to same-repo PRs, not forks ([#77](https://github.com/s3ntin3l8/branchdam/issues/77)) ([c915f9e](https://github.com/s3ntin3l8/branchdam/commit/c915f9efa7ce8a3c2fb443b306a1feb2e7b191c6))
* **cmd,sync:** join the Immich sync worker on shutdown ([#158](https://github.com/s3ntin3l8/branchdam/issues/158)) ([#173](https://github.com/s3ntin3l8/branchdam/issues/173)) ([dd78b8f](https://github.com/s3ntin3l8/branchdam/commit/dd78b8f1100a702af252d354ef8a9c173463d919))
* **graph:** stop edge upsert from aborting resolution on human-locked edges ([#128](https://github.com/s3ntin3l8/branchdam/issues/128)) ([891857e](https://github.com/s3ntin3l8/branchdam/commit/891857e8e01edac72c3901280dd91e69b291a437))
* **graph:** Tier-3 heuristic resolver requires a raw-&gt;export role before emitting a burst-frame candidate ([#187](https://github.com/s3ntin3l8/branchdam/issues/187)) ([baf52fb](https://github.com/s3ntin3l8/branchdam/commit/baf52fbf9347f17a355f5a39641d92f41475a93c))
* **httpapi,shutdown:** stop SSE from being killed by WriteTimeout, fix shutdown-error path skipping joins and DB close ([#138](https://github.com/s3ntin3l8/branchdam/issues/138)) ([f5b2295](https://github.com/s3ntin3l8/branchdam/commit/f5b22957017b149b5cbb63dd37115fd601726d21))
* **httpapi,storage:** bound Statfs with a timeout, stop a vanished mount from bricking startup ([#151](https://github.com/s3ntin3l8/branchdam/issues/151)) ([ba6eee3](https://github.com/s3ntin3l8/branchdam/commit/ba6eee30321e3ec0c9a8c1f5515c5a332a2875e2))
* **httpapi:** confirm/reject edge returns 404 for nonexistent id, recomputes graph_status ([#142](https://github.com/s3ntin3l8/branchdam/issues/142)) ([853a796](https://github.com/s3ntin3l8/branchdam/commit/853a796c56ec0c4252620268204e813c06fb1231))
* **httpapi:** fix inherit-metadata's UTC timestamp fallback and cover loadTagSet ([#184](https://github.com/s3ntin3l8/branchdam/issues/184)) ([#203](https://github.com/s3ntin3l8/branchdam/issues/203)) ([decb0d9](https://github.com/s3ntin3l8/branchdam/commit/decb0d951c022d4064fd4c8947b006440cf53b6e))
* **httpapi:** inherit-metadata refreshes node hash/mtime after write ([#180](https://github.com/s3ntin3l8/branchdam/issues/180)) ([#188](https://github.com/s3ntin3l8/branchdam/issues/188)) ([9fead32](https://github.com/s3ntin3l8/branchdam/commit/9fead324d67bd252940611762e28debdb57900b9))
* **httpapi:** restrict inherit-metadata's parent selection ([#181](https://github.com/s3ntin3l8/branchdam/issues/181)) ([#198](https://github.com/s3ntin3l8/branchdam/issues/198)) ([6306310](https://github.com/s3ntin3l8/branchdam/commit/63063100a92e92bf51adeef0eaeaa37be94909ee))
* **immich:** retry on 429 / respect Retry-After ([#159](https://github.com/s3ntin3l8/branchdam/issues/159)) ([#174](https://github.com/s3ntin3l8/branchdam/issues/174)) ([60361dd](https://github.com/s3ntin3l8/branchdam/commit/60361dd59627bf6b3cb404000a6f697d5a90922f))
* **pipeline:** advance scan progress on the batch interval and nudge SSE from the scan path ([58f7074](https://github.com/s3ntin3l8/branchdam/commit/58f707452917d0389ff16b8d62e24a62500c49f1))
* **pipeline:** bound the watcher event handoff so a burst can't pile up unbounded goroutines ([0841b66](https://github.com/s3ntin3l8/branchdam/commit/0841b667249e3631a8ce73458f37df6a0e65663b))
* **pipeline:** clarify metadata persistence contract, watch-path log ([#33](https://github.com/s3ntin3l8/branchdam/issues/33)) ([bc491de](https://github.com/s3ntin3l8/branchdam/commit/bc491ded8b58dba49b6be335e8becf7d7bd61f19))
* **pipeline:** drain results concurrently with the walk ([c767967](https://github.com/s3ntin3l8/branchdam/commit/c767967a3308e21bb5eb90ac0fc38989895c8e39)), closes [#93](https://github.com/s3ntin3l8/branchdam/issues/93)
* **pipeline:** guard MISSING sweep against failed files and strand-free finalize ([#31](https://github.com/s3ntin3l8/branchdam/issues/31)) ([94d029e](https://github.com/s3ntin3l8/branchdam/commit/94d029e9b3b88bb5fe83d3385446c1a5b5686197))
* **pipeline:** mark unseen nodes MISSING so move detection can fire ([a10c176](https://github.com/s3ntin3l8/branchdam/commit/a10c176916dba15d26838e86f99cbb24a8321988))
* **pipeline:** mark unseen nodes MISSING so move detection can fire ([#31](https://github.com/s3ntin3l8/branchdam/issues/31)) ([be2081a](https://github.com/s3ntin3l8/branchdam/commit/be2081a99c4068a937acd15fdc1784af8763ed24))
* **pipeline:** normalize video-ext gate, shared probe timeout, watch-path note ([#34](https://github.com/s3ntin3l8/branchdam/issues/34)) ([c66946d](https://github.com/s3ntin3l8/branchdam/commit/c66946dc245fc81dafb5bf9fc136ce5d7dce034c))
* **pipeline:** persist node_metadata on touched and rebased nodes, not only on insert ([e10def6](https://github.com/s3ntin3l8/branchdam/commit/e10def66fca220170c7925fcc791dbcf51ef306b)), closes [#86](https://github.com/s3ntin3l8/branchdam/issues/86)
* **pipeline:** retry project-sidecar edge resolution once at end-of-scan ([#191](https://github.com/s3ntin3l8/branchdam/issues/191)) ([746f3d5](https://github.com/s3ntin3l8/branchdam/commit/746f3d53f766c6b5e1a29cdeb42e4532fe16b3d0))
* **pipeline:** run scans beyond the request context and exclude uncertain paths from the MISSING sweep ([#31](https://github.com/s3ntin3l8/branchdam/issues/31)) ([8e53462](https://github.com/s3ntin3l8/branchdam/commit/8e534620fe3c12288e19d2260a6d5b45ccaea2e9))
* **pipeline:** serialized watch consumer, order-independent renames ([#32](https://github.com/s3ntin3l8/branchdam/issues/32)) ([680b15b](https://github.com/s3ntin3l8/branchdam/commit/680b15b10355614c0beb6d62935f4dd7e9fbf638))
* **pipeline:** stop filenameStem from collapsing camera default filenames into one auto-accepted mesh ([#134](https://github.com/s3ntin3l8/branchdam/issues/134)) ([bd0490c](https://github.com/s3ntin3l8/branchdam/commit/bd0490cb75c5f4b18bdb7086ed29834ebe138f35)), closes [#133](https://github.com/s3ntin3l8/branchdam/issues/133)
* **pipeline:** thread an edgesCreated counter through the watch path so it stops resetting to 0 ([#140](https://github.com/s3ntin3l8/branchdam/issues/140)) ([fa5e452](https://github.com/s3ntin3l8/branchdam/commit/fa5e452f3a1e756db13fdb364bd9d2879a4d5936))
* **pipeline:** thread scan start time and walk seam for the MISSING sweep (phase 1 [#31](https://github.com/s3ntin3l8/branchdam/issues/31)) ([8ea9117](https://github.com/s3ntin3l8/branchdam/commit/8ea91175edfe80177becd03b699cd3a3d11d982d))
* **pipeline:** track the runScan goroutine and join it before db.Close ([85c4ea7](https://github.com/s3ntin3l8/branchdam/commit/85c4ea74049f83e83a99ab579ef89135290b5eec)), closes [#92](https://github.com/s3ntin3l8/branchdam/issues/92)
* **pipeline:** warn on Start re-entry, nudge only on real changes, correct docs ([#32](https://github.com/s3ntin3l8/branchdam/issues/32)) ([57644ae](https://github.com/s3ntin3l8/branchdam/commit/57644aec52296f9a993337e769178c0505340129))
* **pruning:** re-verify on-disk file freshness before Guard.Remove ([#196](https://github.com/s3ntin3l8/branchdam/issues/196)) ([7c5bfb0](https://github.com/s3ntin3l8/branchdam/commit/7c5bfb09c24ff55ad467c855faa1b06c35a04aee))
* **shutdown:** skip database.Close() after a timed-out wait, unblock SSE on shutdown ([#125](https://github.com/s3ntin3l8/branchdam/issues/125)) ([2632ba1](https://github.com/s3ntin3l8/branchdam/commit/2632ba12f1ed4b531ca0b3a2357f075613f85eac))
* **sync:** make Enqueue atomic and truly idempotent for PENDING ([#131](https://github.com/s3ntin3l8/branchdam/issues/131)) ([035acd6](https://github.com/s3ntin3l8/branchdam/commit/035acd6ff3a89c01cdd188f8668e4408a3fe38c6))
* **sync:** trigger at most one Immich scan per drain tick ([#183](https://github.com/s3ntin3l8/branchdam/issues/183)) ([#202](https://github.com/s3ntin3l8/branchdam/issues/202)) ([c2e42d9](https://github.com/s3ntin3l8/branchdam/commit/c2e42d976cf8baf999d3bc04c5d538b2ed854ece))
* **web:** confirm/reject/create-edge and SSE nudges under-invalidate TanStack Query cache ([#155](https://github.com/s3ntin3l8/branchdam/issues/155)) ([482864e](https://github.com/s3ntin3l8/branchdam/commit/482864e4b9398b3fc36b75986e5051c93dc1e025))
* **web:** error-state handling for ingest queries, DTO naming, backend test hardening ([#35](https://github.com/s3ntin3l8/branchdam/issues/35)) ([166a0d9](https://github.com/s3ntin3l8/branchdam/commit/166a0d9c2594c789cba56d7cd126c9d766aa427b))
* **web:** surface mutation failures for startScan and the audit-queue actions ([#91](https://github.com/s3ntin3l8/branchdam/issues/91)) ([#120](https://github.com/s3ntin3l8/branchdam/issues/120)) ([9078740](https://github.com/s3ntin3l8/branchdam/commit/907874057065376f2834defcaf75d1d3611ba6bd))


### Performance Improvements

* **db:** add a covering index for remote_sync_state's remote+sync_status queries ([#168](https://github.com/s3ntin3l8/branchdam/issues/168)) ([a703531](https://github.com/s3ntin3l8/branchdam/commit/a70353111283ca3b762e9f1fe549949f5ac52c93))
* **pipeline:** skip rewriting unchanged node_metadata rows on touch/rebase ([08c0515](https://github.com/s3ntin3l8/branchdam/commit/08c05153c240365ef610b81ea92823d8e94ad115))

## [0.2.0](https://github.com/s3ntin3l8/branchdam/compare/v0.1.0...v0.2.0) (2026-08-12)


### Features

* **api:** Huma REST API, SSE progress, full startup wiring (PR 9) ([#9](https://github.com/s3ntin3l8/branchdam/issues/9)) ([7f29ec7](https://github.com/s3ntin3l8/branchdam/commit/7f29ec7a5f417c7e8b8126e4dc8857bce00c1760))
* **auth:** BrowserChain/AgentChain, structurally-safe header handling (PR 8) ([#8](https://github.com/s3ntin3l8/branchdam/issues/8)) ([8e6e740](https://github.com/s3ntin3l8/branchdam/commit/8e6e74023c5d3bd35db1e90af07e67a5ff035ec8))
* **db:** corrected schema, goose migrations, sqlc, dual connection pool ([#1](https://github.com/s3ntin3l8/branchdam/issues/1)) ([475ea79](https://github.com/s3ntin3l8/branchdam/commit/475ea79f7102fc112f2259a0ee7213f663bc2f6e))
* **docker:** Dockerfile, compose (PR 11) ([#13](https://github.com/s3ntin3l8/branchdam/issues/13)) ([2f1cea5](https://github.com/s3ntin3l8/branchdam/commit/2f1cea533f84c6fedc5285799b4ce8b60295b5f4))
* **graph:** Tier-2 edge resolution, cycle guard, human-review upsert (PR 7) ([#7](https://github.com/s3ntin3l8/branchdam/issues/7)) ([49d87e0](https://github.com/s3ntin3l8/branchdam/commit/49d87e04eb214e481e9b5a9b44c461dd40f8d27f))
* **hashing:** fast/full/perceptual hashing, pure functions (PR 3) ([#3](https://github.com/s3ntin3l8/branchdam/issues/3)) ([0e86e67](https://github.com/s3ntin3l8/branchdam/commit/0e86e6733da6528f184c9b58e0a6f784e9ffa564))
* **pipeline:** scan orchestration, batched writes, collision handling (PR 6) ([#6](https://github.com/s3ntin3l8/branchdam/issues/6)) ([7b0fbca](https://github.com/s3ntin3l8/branchdam/commit/7b0fbca2f4d8a6d6cd8fad9f50abcca07ca219e1))
* **probe:** exiftool/ffprobe subprocess wrappers (PR 5) ([#5](https://github.com/s3ntin3l8/branchdam/issues/5)) ([950c7ce](https://github.com/s3ntin3l8/branchdam/commit/950c7ce93c8d72cf1f7fe8f92d260357499c6e04))
* **storage:** Guard, the single write chokepoint for Tier 3 (PR 2) ([#2](https://github.com/s3ntin3l8/branchdam/issues/2)) ([346adc0](https://github.com/s3ntin3l8/branchdam/commit/346adc008ff56f22cd75d2304deb25b9b747ebdc))
* **web:** React 19 + Vite SPA ([#10](https://github.com/s3ntin3l8/branchdam/issues/10)) ([eedef90](https://github.com/s3ntin3l8/branchdam/commit/eedef901f553dc307cfe408ab91a9583fbea01e0))
* **workers,indexer:** bounded goroutine pool, directory walk, fsnotify watch (PR 4) ([#4](https://github.com/s3ntin3l8/branchdam/issues/4)) ([3116c31](https://github.com/s3ntin3l8/branchdam/commit/3116c313a735fc926f1b9c4707fe12f4ce99983d))
