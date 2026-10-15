import { execFileSync } from "node:child_process";
import { copyFileSync, mkdirSync, renameSync, writeFileSync } from "node:fs";
import path from "node:path";

/**
 * Synthetic media for the demo: rendered landscapes (ffmpeg) with real camera
 * metadata (exiftool), laid out so branchDAM's own resolvers build a version
 * graph from it (internal/graph/resolvers.go, thresholds in engine.go):
 *
 *   camera JPEG next to its RAW (same stem, dir, day, body)   0.90 auto-accepted
 *   graded export carrying the RAW's XMP DocumentID           0.95 auto-accepted
 *   EDL project referencing the RAWs                          1.00 auto-accepted
 *   same-stem copy in another folder                          0.85 review queue
 *   renamed client export matched by serial + capture time    ~0.79 review queue
 *
 * "RAW" files are JPEG data with a camera-raw extension: any file can be a
 * graph root, and branchDAM detects the image format from content. Nothing here
 * is a real photo, person, place or device.
 */

// [top sky, horizon, sun, near hills, far hills] as RGB.
const PALETTES = {
  alpine: [[60, 110, 170], [200, 225, 240], [255, 255, 235], [70, 90, 110], [40, 55, 70]],
  ocean: [[10, 40, 70], [120, 190, 210], [255, 248, 220], [25, 70, 90], [10, 35, 50]],
  desert: [[90, 60, 110], [250, 170, 110], [255, 235, 190], [150, 80, 60], [90, 45, 40]],
  dusk: [[24, 30, 66], [255, 140, 90], [255, 226, 170], [70, 40, 70], [30, 18, 40]],
};

export const SHOOTS = [
  { slug: "dolomites", title: "Dolomites", palette: "alpine", date: "2026:09:14", first: 1101, frames: 6, body: 0 },
  { slug: "lisbon-coast", title: "Lisbon coast", palette: "ocean", date: "2026:08:02", first: 1201, frames: 5, body: 1 },
  { slug: "atacama", title: "Atacama", palette: "desert", date: "2026:07:19", first: 1301, frames: 5, body: 0 },
  { slug: "midsummer", title: "Midsummer", palette: "dusk", date: "2026:06:28", first: 1401, frames: 6, body: 1 },
];

const BODIES = [
  { make: "SONY", model: "ILCE-7RM5", serial: "4521337", lens: "FE 24-70mm F2.8 GM II", ext: "ARW" },
  { make: "FUJIFILM", model: "X-T5", serial: "2KQ04117", lens: "XF16-55mmF2.8 R LM WR", ext: "RAF" },
];

function landscape(file, palette, seed, grade = "") {
  const [top, bot, sun, h1, h2] = PALETTES[palette];
  const sx = 0.25 + 0.5 * ((seed * 0.37) % 1);
  const sy = 0.34 + 0.14 * ((seed * 0.53) % 1);
  const p = seed * 1.7;
  const channel = (c) => {
    const sky = `(${top[c]}+(${bot[c]}-${top[c]})*min(Y/(H*0.68),1))`;
    const near = `gt(Y,H*(0.60+0.07*sin(2*PI*X/W*1.6+${p})+0.025*sin(2*PI*X/W*5.1+${p * 2})))`;
    const far = `gt(Y,H*(0.74+0.05*sin(2*PI*X/W*2.4+${p + 1.3})+0.02*sin(2*PI*X/W*7.0+${p})))`;
    const disc = `lt(hypot(X-W*${sx},Y-H*${sy}),H*0.06)`;
    const glow = `(${sun[c]}-${sky})*0.35*exp(-hypot(X-W*${sx},Y-H*${sy})/(H*0.18))`;
    return `if(${far},${h2[c]}+8*Y/H,if(${near},${h1[c]}+14*Y/H,if(${disc},${sun[c]},${sky}+${glow})))`;
  };
  const filters = [`geq=r='${channel(0)}':g='${channel(1)}':b='${channel(2)}'`, "noise=alls=3:allf=t"];
  if (grade) filters.push(grade);
  execFileSync("ffmpeg", [
    "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=s=1200x800:c=black",
    "-vf", filters.join(","), "-frames:v", "1", "-q:v", "4", file,
  ]);
}

function exif(file, tags) {
  execFileSync("exiftool", ["-q", "-overwrite_original", ...Object.entries(tags).map(([k, v]) => `-${k}=${v}`), file]);
}

const pad = (n) => String(n).padStart(2, "0");

/** Writes the whole dataset under the given storage roots; returns a summary. */
export function generateMedia(roots) {
  const counts = { raw: 0, cameraJpeg: 0, export: 0, review: 0, project: 0, proxy: 0 };
  for (const shoot of SHOOTS) {
    const body = BODIES[shoot.body];
    const year = shoot.date.slice(0, 4);
    const rawDir = path.join(roots.archive, year, shoot.slug);
    const exportDir = path.join(roots.exports, shoot.slug);
    mkdirSync(rawDir, { recursive: true });
    mkdirSync(exportDir, { recursive: true });
    const raws = [];

    for (let i = 0; i < shoot.frames; i++) {
      const stem = `DSC0${shoot.first + i}`;
      const seed = shoot.first + i;
      const time = `${shoot.date} ${pad(17 + Math.floor(i / 3))}:${pad((i * 7) % 60)}:${pad((i * 13) % 60)}`;
      const camera = {
        "EXIF:Make": body.make,
        "EXIF:Model": body.model,
        "EXIF:SerialNumber": body.serial,
        "EXIF:LensModel": body.lens,
        "EXIF:DateTimeOriginal": time,
        "EXIF:ISO": 100 + 100 * (i % 3),
        "EXIF:FNumber": [2.8, 4, 5.6, 8][i % 4],
        "EXIF:ExposureTime": ["1/250", "1/500", "1/125", "1/60"][i % 4],
      };
      const documentId = `xmp.did:demo-${shoot.slug}-${i + 1}`;

      // The original. Written as .jpg so exiftool knows the format, then renamed.
      const work = path.join(rawDir, `${stem}.jpg`);
      landscape(work, shoot.palette, seed);
      exif(work, { ...camera, "XMP-xmpMM:DocumentID": documentId });
      const raw = path.join(rawDir, `${stem}.${body.ext}`);
      renameSync(work, raw);
      raws.push(`${stem}.${body.ext}`);
      counts.raw++;

      // Camera JPEG beside it: same stem, folder, day and body (0.90).
      if (i < 3) {
        const jpeg = path.join(rawDir, `${stem}.JPG`);
        landscape(jpeg, shoot.palette, seed);
        exif(jpeg, camera);
        counts.cameraJpeg++;
      }
      // Graded export that knows its original by XMP DocumentID (0.95).
      if (i < 2) {
        const graded = path.join(exportDir, `${stem}_edit.jpg`);
        landscape(graded, shoot.palette, seed, "eq=contrast=1.12:saturation=1.25:gamma=0.95");
        exif(graded, { ...camera, "XMP-xmpMM:OriginalDocumentID": documentId });
        counts.export++;
      }
      // Same-stem web copy in a different folder: plausible but unproven (0.85).
      if (i === 2) {
        const web = path.join(roots.exports, "web", `${stem}.jpg`);
        mkdirSync(path.dirname(web), { recursive: true });
        landscape(web, shoot.palette, seed, "scale=800:-1");
        exif(web, { "EXIF:Model": body.model, "EXIF:DateTimeOriginal": time });
        counts.review++;
      }
      // Renamed client delivery: only the body's serial and the capture time
      // connect it to the original (~0.79).
      if (i === 3) {
        const client = path.join(roots.exports, "client", `${shoot.slug}-hero.jpg`);
        mkdirSync(path.dirname(client), { recursive: true });
        landscape(client, shoot.palette, seed);
        exif(client, camera);
        counts.review++;
      }
    }

    // An edit decision list referencing the first frames (1.00), plus a short
    // proxy clip of the lead frame.
    const projectDir = path.join(roots.projects, shoot.slug);
    mkdirSync(projectDir, { recursive: true });
    const edl = [`TITLE: ${shoot.title.toUpperCase()} SELECTS`, "FCM: NON-DROP FRAME", ""];
    raws.slice(0, 3).forEach((name, n) => {
      const tc = `00:00:${pad(n * 4)}:00 00:00:${pad(n * 4 + 4)}:00`;
      edl.push(`${String(n + 1).padStart(3, "0")}  AX       V     C        ${tc} 01:00:${pad(n * 4)}:00 01:00:${pad(n * 4 + 4)}:00`);
      edl.push(`* FROM CLIP NAME: ${name}`, `* SOURCE FILE: ${name}`, "");
    });
    writeFileSync(path.join(projectDir, `${shoot.slug}-selects.edl`), `${edl.join("\n")}\n`);
    counts.project++;

    const lead = raws[0].replace(/\.\w+$/, "");
    const still = path.join(projectDir, ".proxy-frame.jpg");
    copyFileSync(path.join(rawDir, raws[0]), still);
    execFileSync("ffmpeg", [
      "-loglevel", "error", "-y", "-loop", "1", "-t", "3", "-i", still,
      "-vf", "scale=640:-2,format=yuv420p", "-c:v", "libx264", "-r", "24",
      path.join(projectDir, `${lead}_proxy.mp4`),
    ]);
    execFileSync("rm", ["-f", still]);
    counts.proxy++;
  }
  return counts;
}
