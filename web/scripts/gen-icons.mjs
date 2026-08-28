// Derives the whole favicon/PWA/social-image asset set from the single
// master SVG at src/assets/brand-mark.svg, so the committed PNGs/ICO have
// reproducible provenance instead of being opaque binaries. Run after any
// change to the master mark:
//
//   node scripts/gen-icons.mjs
//
// Outputs land in public/, which Vite copies verbatim into dist/ and
// web/embed.go (`//go:embed all:dist`) then embeds into the Go binary.
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import sharp from "sharp";
import pngToIco from "png-to-ico";

const root = path.dirname(fileURLToPath(import.meta.url));
const webRoot = path.resolve(root, "..");
const srcSvg = path.join(webRoot, "src/assets/brand-mark.svg");
const outDir = path.join(webRoot, "public");

const BRAND = "#6366f1"; // indigo -- see src/styles/index.css --color-brand
const CANVAS = "#0a0a0b"; // matches body bg-neutral-950 / dead --color-canvas token

mkdirSync(outDir, { recursive: true });

const maskSvg = readFileSync(srcSvg, "utf8");

// The master SVG uses currentColor; for standalone raster/SVG files we bake
// in the fixed brand color since there's no CSS context to inherit from.
function colored(svg, color) {
  return svg.replace(/currentColor/g, color);
}

async function pngBuffer(size, { tile = false, inset = 1 } = {}) {
  const markSize = tile ? Math.round(size * inset) : size;
  const markSvg = colored(maskSvg, tile ? CANVAS : BRAND);
  const mark = await sharp(Buffer.from(markSvg)).resize(markSize, markSize).png().toBuffer();

  if (!tile) return mark;

  // App-tile / maskable variants: mark centered on a solid brand-color
  // square, inset so Android's adaptive-icon mask (~66% safe zone for
  // maskable, more relaxed for a plain square tile) never crops the glyph.
  const offset = Math.round((size - markSize) / 2);
  return sharp({
    create: { width: size, height: size, channels: 4, background: BRAND },
  })
    .composite([{ input: mark, left: offset, top: offset }])
    .png()
    .toBuffer();
}

async function main() {
  // Plain indigo favicon SVG (fixed color -- browsers render this outside
  // any app CSS context, so currentColor would resolve to black).
  writeFileSync(path.join(outDir, "favicon.svg"), colored(maskSvg, BRAND));

  // Multi-resolution favicon.ico for legacy browsers / Windows pinned sites.
  const icoSizes = [16, 32, 48];
  const icoPngs = await Promise.all(icoSizes.map((s) => pngBuffer(s)));
  writeFileSync(path.join(outDir, "favicon.ico"), await pngToIco(icoPngs));

  // Apple touch icon: opaque tile, no transparency (iOS ignores alpha and
  // would otherwise composite onto an unpredictable background).
  writeFileSync(path.join(outDir, "apple-touch-icon.png"), await pngBuffer(180, { tile: true, inset: 0.62 }));

  // PWA icons.
  writeFileSync(path.join(outDir, "icon-192.png"), await pngBuffer(192, { tile: true, inset: 0.62 }));
  writeFileSync(path.join(outDir, "icon-512.png"), await pngBuffer(512, { tile: true, inset: 0.62 }));

  // Maskable PWA icon: mark inset to the ~80% safe zone (40% radius from
  // center) per the maskable-icon spec, so Android's circular/squircle mask
  // doesn't clip the glyph.
  writeFileSync(path.join(outDir, "icon-maskable-512.png"), await pngBuffer(512, { tile: true, inset: 0.5 }));

  // Open Graph / Twitter card image: mark + wordmark on the app's own dark
  // canvas color, sized to the standard 1200x630 social-preview aspect.
  const ogMarkSize = 160;
  const ogMark = await sharp(Buffer.from(colored(maskSvg, BRAND))).resize(ogMarkSize, ogMarkSize).png().toBuffer();
  const ogSvgText = `
    <svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630">
      <rect width="1200" height="630" fill="${CANVAS}"/>
      <text x="600" y="430" font-family="system-ui, -apple-system, sans-serif" font-size="72"
            font-weight="600" fill="#e5e5e5" text-anchor="middle">
        <tspan font-weight="400">branch</tspan><tspan fill="${BRAND}">DAM</tspan>
      </text>
      <text x="600" y="480" font-family="system-ui, -apple-system, sans-serif" font-size="24"
            fill="#8a8a8a" text-anchor="middle">Self-hosted digital asset management with version lineage graphs</text>
    </svg>`;
  await sharp(Buffer.from(ogSvgText))
    .composite([{ input: ogMark, left: (1200 - ogMarkSize) / 2, top: 150 }])
    .png()
    .toFile(path.join(outDir, "og-image.png"));

  writeFileSync(
    path.join(outDir, "site.webmanifest"),
    JSON.stringify(
      {
        name: "branchDAM",
        short_name: "branchDAM",
        description: "Self-hosted digital asset management with version lineage graphs",
        icons: [
          { src: "/icon-192.png", sizes: "192x192", type: "image/png" },
          { src: "/icon-512.png", sizes: "512x512", type: "image/png" },
          { src: "/icon-maskable-512.png", sizes: "512x512", type: "image/png", purpose: "maskable" },
        ],
        theme_color: BRAND,
        background_color: CANVAS,
        display: "standalone",
      },
      null,
      2,
    ) + "\n",
  );

  console.log("Wrote icon set to", path.relative(webRoot, outDir));
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
