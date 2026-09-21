# Asset provenance

Every asset in this repository is original to the Yukariko project.

| Asset | Origin |
|---|---|
| `internal/ui/static/style.css` | Hand-written for Yukariko. The locked palette is light lapis quiet software: canvas `#F7F3EB`, surface `#FFFBF5`, ink `#1A1A18`, lapis `#2E5EA8`, mirage `#A8C4E8`, rose-gold `#B8956C`, ok `#2F6F5E`, fail `#9E3B3B` (stale `#6b7280` kept for remote-host age). The tiny faceted diamond mark and the Updating spinner are original CSS/SVG. No frameworks, resets, or third-party stylesheets were copied or derived. |
| `internal/ui/static/live.js` | Hand-written for Yukariko. Progressive enhancement: same-origin GET of the current page, swap `#main` and `footer`, drop the meta refresh. No libraries, no mutation, no third-party URLs. |
| `internal/ui/templates/*.html` | Hand-written Go `html/template` files. No third-party layouts or component libraries. The visible chrome is Status / Versions / Health / Logs with a Chronicle side panel; template filenames remain sanctuary / vestments / divination / chronicle. Live updates: meta refresh without JS, `live.js` fragment swap with JS. |
| No images, fonts, logos, or audio | The dashboard is typography, CSS, and a four-polygon inline SVG mark only. In particular there are **no** Mai-HiME/Mai-Otome (or any third-party) assets, characters, logos, or marks: the Yukariko name and its presentation are original to this project. |

Any future asset must be added to this inventory with its origin and license,
or replaced by an original work.
