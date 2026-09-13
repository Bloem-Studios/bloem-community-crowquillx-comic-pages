# Fixture provenance

These fixtures contain original generated PNGs, with no downloaded comic artwork.
They were copied unchanged from [aidoku-silo-sources](https://github.com/crowquillx/aidoku-silo-sources/tree/dbb183969ec7abb069b3da2bb7e1fd547d8662ee/experiments/cbr-wasm),
using its MIT licensing option; the [copyright and license text](LICENSE-MIT) is retained.

The [generator](https://github.com/crowquillx/aidoku-silo-sources/blob/dbb183969ec7abb069b3da2bb7e1fd547d8662ee/experiments/cbr-wasm/generate-fixtures.cjs)
uses `@bitplane/rars` 0.9.4, compression level 3, RAR4 and RAR5, with solid mode
on and off. Each archive contains page10.png, page2.png, page1.png in that order.
Images are 128 × 128, each 49,348 bytes, with uncompressed PNG IDAT content.
Timestamps are fixed to 2024-01-01 UTC. No proprietary RAR writer was needed.

The [independent verifier](https://github.com/crowquillx/aidoku-silo-sources/blob/dbb183969ec7abb069b3da2bb7e1fd547d8662ee/experiments/cbr-wasm/verify-fixtures.cjs)
uses `node-unrar-js` 2.0.2. The Go tests compare every extracted image against
the PNG originals. WebP fixture provenance is [documented separately](../WEBP.md).

| File | Bytes | SHA-256 |
| --- | ---: | --- |
| page1.png | 49348 | `406ed69887e122fb6e8f892d234455970e2b3a07cd101f9594252e5346430528` |
| page10.png | 49348 | `e111184e0d7ad18045b4c6070536c7ca04699ba0b45aff22393fb2765680011f` |
| page2.png | 49348 | `860b0e869588a265f9bc958e0345629cc5599a33fdb728a5e47caa76972706d8` |
| rar40-normal.cbr | 5999 | `1ae0d036027a484abf7b5daaf28222096138be9a4fc923c7ecfb3c62e4a13054` |
| rar40-solid.cbr | 4040 | `baa23c8986eccacd92296ed293c9c230ce3be1f200edc8544f7786598e87a934` |
| rar50-normal.cbr | 4815 | `394850acaf79012dd0ffbbe128492526e4f1290029d4246c9e4cca329bd54cbf` |
| rar50-solid.cbr | 127742 | `4d3dee34b9a495cf1ac820375f51808fb242a1e244496c23eb7a982f4ee77d1e` |
