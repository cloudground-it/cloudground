# CloudGround icons

`cg/` holds the 120 SVGs of [cloudground-it/cloudground-icons](https://github.com/cloudground-it/cloudground-icons)
(MIT, see `LICENSE`), copied byte for byte from that repo's `icons/` directory. Do not edit them
here; change the icon in its repo and copy it again.

`nuxt.config.ts` registers the directory as the Iconify collection `cg` (`icon.customCollections`),
so an icon is `i-cg-<file name>`, e.g. `<UIcon name="i-cg-backup" />`. The whole collection is
bundled into the client (`clientBundle.includeCustomCollections`) and `provider: 'none'` keeps
@nuxt/icon from fetching anything at runtime. `app.config.ts` points Nuxt UI's own icons
(`ui.icons`) at it as well.

To refresh:

```sh
rm ui/app/assets/icons/cg/*.svg
cp ../cloudground-icons/icons/*.svg ui/app/assets/icons/cg/
cp ../cloudground-icons/LICENSE ui/app/assets/icons/LICENSE
make docker-dist   # rebuilds ui/dist; commit it with the icons
```

An icon the interface uses that the set drops shows as an empty box: grep `i-cg-` for it first.
