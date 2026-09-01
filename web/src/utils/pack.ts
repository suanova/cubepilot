// Pack a picked skill directory into the gzip tar bytes the publish API
// accepts (SKILL.md at the archive root; scripts/ and references/ preserved).
// Browsers cannot produce a tar natively: fflate supplies gzipSync, and the
// ustar writer below is the tar format for regular files (a 512-byte header
// per file + the file content padded to 512, then a zero trailer).
import { gzipSync } from 'fflate'

// PackError is thrown by packSkillDir for selections that are not a valid
// skill directory (no SKILL.md at the root) or with a path too long for ustar.
export class PackError extends Error {}

const BLOCK = 512

// ustarHeader builds the 512-byte POSIX ustar header for one regular file.
// Long paths are split into the ustar `prefix` (<=155) + `name` (<=100) on a
// '/' boundary; the checksum is the byte sum of the header with the checksum
// field held as spaces, written as 6 octal digits + NUL + space.
function ustarHeader(path: string, size: number): Uint8Array {
  const enc = new TextEncoder()
  let prefix = ''
  let name = path
  if (enc.encode(path).length > 100) {
    // Split on a '/' such that the encoded prefix fits the 155-byte ustar
    // field and the encoded name fits the 100-byte field — measuring both
    // with TextEncoder, so a multibyte prefix throws PackError instead of
    // letting h.set overflow (RangeError).
    let split = -1
    for (let i = path.length - 1; i >= 0; i--) {
      if (
        path[i] === '/' &&
        enc.encode(path.slice(0, i)).length <= 155 &&
        enc.encode(path.slice(i + 1)).length <= 100
      ) {
        split = i
        break
      }
    }
    if (split === -1) throw new PackError(`skill path too long for the tar format: ${path}`)
    prefix = path.slice(0, split)
    name = path.slice(split + 1)
  }
  const h = new Uint8Array(BLOCK)
  h.set(enc.encode(name), 0)
  h.set(enc.encode('0000644\0'), 100) // mode 0644
  h.set(enc.encode('0000000\0'), 108) // uid 0
  h.set(enc.encode('0000000\0'), 116) // gid 0
  h.set(enc.encode(size.toString(8).padStart(11, '0') + '\0'), 124) // size
  h.set(enc.encode('00000000000\0'), 136) // mtime 0
  h[156] = 0x30 // typeflag '0' (regular file)
  h.set(enc.encode('ustar\0'), 257)
  h.set(enc.encode('00'), 263)
  if (prefix) h.set(enc.encode(prefix), 345)
  let sum = 0
  for (let i = 0; i < BLOCK; i++) sum += h[i]
  for (let i = 148; i < 156; i++) sum += 0x20 // checksum field is 8 spaces
  h.set(enc.encode(sum.toString(8).padStart(6, '0') + '\0 '), 148)
  return h
}

// packSkillDir packs the File[] from an <input type="file" webkitdirectory>
// selection into gzip tar bytes, stripping the leading folder segment so the
// archive root holds SKILL.md / scripts / references directly.
export async function packSkillDir(files: File[]): Promise<Uint8Array<ArrayBuffer>> {
  const fileEntries: { path: string; data: Uint8Array }[] = []
  let hasSkillMd = false
  for (const f of files) {
    const slash = f.webkitRelativePath.indexOf('/')
    if (slash === -1) continue // ignore files with no directory info
    const path = f.webkitRelativePath.slice(slash + 1)
    if (path === 'SKILL.md') hasSkillMd = true
    fileEntries.push({ path, data: new Uint8Array(await f.arrayBuffer()) })
  }
  if (!hasSkillMd) {
    throw new PackError('The selected folder has no SKILL.md at its root')
  }

  // Lay out the tar: per file a 512-byte header + content padded to 512, then
  // the two-zero-block trailer.
  let total = BLOCK * 2
  for (const e of fileEntries) total += BLOCK + Math.ceil(e.data.length / BLOCK) * BLOCK
  const out = new Uint8Array(total)
  let off = 0
  for (const e of fileEntries) {
    out.set(ustarHeader(e.path, e.data.length), off)
    off += BLOCK
    out.set(e.data, off)
    off += Math.ceil(e.data.length / BLOCK) * BLOCK
  }
  return gzipSync(out)
}
