Silo Comic Pages adds server extraction for CBR comics without changing Silo or
the Aidoku app. RAR4/RAR5 and solid archives are supported, with authenticated
page requests and a bounded cache.

Download `plugin-linux-amd64` or `plugin-linux-arm64` for your server. Upload it
under Silo Administration → Plugins, or install through `repository.json`.
Configure the Silo base URL and a dedicated writable cache directory. Then
update the Aidoku Silo source to v5 and enter the plugin installation ID in its
Reading settings. See [installation instructions](https://github.com/crowquillx/silo-comic-pages#install).

Defaults: 512 MiB archive, 32 MiB page, 1 GiB extracted data, 2,048 entries,
4 GiB cache, and one extraction job at a time. Each child has a 120-second job
deadline and a 2 GiB virtual-address-space limit. Encrypted/multipart archives
and animated WebP are unsupported. See [all limits](https://github.com/crowquillx/silo-comic-pages#limits).

The release includes SHA-256 checksums and dependency license texts. Tests cover
the actual Silo SDK gRPC process, original RAR4/RAR5 fixtures, chunked images,
cached-page authorization, and cache lifecycle. Linux arm64 was cross-compiled;
amd64 was executed. See [validation and measurements](https://github.com/crowquillx/silo-comic-pages/blob/main/docs/validation.md).

This is an installable release. It has not been installed on a live Silo server
or validated on an iOS device.
