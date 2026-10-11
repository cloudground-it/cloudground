/**
 * The off-site backup destination, in the words of the Settings page: a
 * kind of storage and the few fields it needs, turned into the restic
 * repository string the API stores (backup_repository) and the region it
 * takes (backup_s3_region). The presets only fill in an endpoint: the API
 * and what it stores are the same as with a repository typed by hand.
 *
 * A stored repository that does not read back to exactly the same string
 * (credentials in the URL, a path the presets do not make, SFTP, REST…) is
 * shown as "advanced": the string as it is, edited by hand.
 */

export type DestinationKind = 'local' | 'aws' | 'r2' | 'b2' | 'wasabi' | 'hetzner' | 'ovh' | 'other' | 'advanced'

export interface Destination {
  kind: DestinationKind
  /** local: the folder on this server. */
  path: string
  /** aws, b2, wasabi, hetzner, ovh: the region or location; other: optional. */
  region: string
  /** r2: the Cloudflare account ID. */
  account: string
  /** other: the endpoint, with or without https://. */
  endpoint: string
  bucket: string
  /** A folder inside the bucket, optional. */
  prefix: string
  /** advanced: the repository string as typed. */
  raw: string
}

interface Preset {
  /** The endpoint host for a region (or account). */
  host: (v: string) => string
  /** Recognises a host, returning the region (or account) it was made for. */
  match: RegExp
  /** What the region field holds: a region, or R2's account ID. */
  field: 'region' | 'account'
  /** The region sent as backup_s3_region when it is not the field. */
  fixedRegion?: string
  /** An example value for the field. */
  example: string
}

export const DESTINATION_PRESETS: Record<'aws' | 'r2' | 'b2' | 'wasabi' | 'hetzner' | 'ovh', Preset> = {
  aws: { host: r => `s3.${r}.amazonaws.com`, match: /^s3\.([a-z0-9-]+)\.amazonaws\.com$/, field: 'region', example: 'eu-central-1' },
  r2: { host: a => `${a}.r2.cloudflarestorage.com`, match: /^([a-f0-9]+)\.r2\.cloudflarestorage\.com$/, field: 'account', fixedRegion: 'auto', example: '0123456789abcdef0123456789abcdef' },
  b2: { host: r => `s3.${r}.backblazeb2.com`, match: /^s3\.([a-z0-9-]+)\.backblazeb2\.com$/, field: 'region', example: 'eu-central-003' },
  wasabi: { host: r => `s3.${r}.wasabisys.com`, match: /^s3\.([a-z0-9-]+)\.wasabisys\.com$/, field: 'region', example: 'eu-central-1' },
  hetzner: { host: r => `${r}.your-objectstorage.com`, match: /^([a-z0-9-]+)\.your-objectstorage\.com$/, field: 'region', example: 'fsn1' },
  ovh: { host: r => `s3.${r}.io.cloud.ovh.net`, match: /^s3\.([a-z0-9-]+)\.io\.cloud\.ovh\.net$/, field: 'region', example: 'gra' }
}

export const DESTINATION_KINDS: DestinationKind[] = ['local', 'aws', 'r2', 'b2', 'wasabi', 'hetzner', 'ovh', 'other', 'advanced']

export const isS3Destination = (k: DestinationKind) => k !== 'local' && k !== 'advanced'

export function blankDestination(kind: DestinationKind = 'r2'): Destination {
  return { kind, path: '', region: '', account: '', endpoint: '', bucket: '', prefix: '', raw: '' }
}

const clean = (v: string) => v.trim().replace(/^\/+|\/+$/g, '')

/** The repository string for a destination; '' while a required field is empty. */
export function repositoryOf(d: Destination): string {
  if (d.kind === 'advanced') return d.raw.trim()
  if (d.kind === 'local') {
    const p = d.path.trim()
    return p.startsWith('/') ? p.replace(/\/+$/, '') || '/' : ''
  }
  const bucket = clean(d.bucket)
  if (!bucket) return ''
  let host: string
  if (d.kind === 'other') {
    host = d.endpoint.trim().replace(/^https?:\/\//, '').replace(/\/+$/, '')
  } else {
    const p = DESTINATION_PRESETS[d.kind]
    const v = (p.field === 'account' ? d.account : d.region).trim()
    if (!v) return ''
    host = p.host(v)
  }
  if (!host) return ''
  const prefix = clean(d.prefix)
  return `s3:https://${host}/${bucket}${prefix ? `/${prefix}` : ''}`
}

/** The region sent with the repository (backup_s3_region). */
export function regionOf(d: Destination): string {
  if (!isS3Destination(d.kind)) return ''
  if (d.kind === 'other') return d.region.trim()
  return DESTINATION_PRESETS[d.kind].fixedRegion ?? d.region.trim()
}

/** Reads a stored repository (and region) back into a destination. */
export function destinationOf(repo: string, region = ''): Destination {
  const r = repo.trim()
  if (!r) return blankDestination()
  const back = (d: Destination) => repositoryOf(d) === r ? d : { ...blankDestination('advanced'), raw: r }
  if (r.startsWith('/')) return back({ ...blankDestination('local'), path: r })
  const m = /^s3:https:\/\/([^/@]+)\/([^/]+)(?:\/(.+))?$/.exec(r)
  if (!m) return { ...blankDestination('advanced'), raw: r }
  const [, host = '', bucket = '', prefix = ''] = m
  for (const [kind, p] of Object.entries(DESTINATION_PRESETS) as [keyof typeof DESTINATION_PRESETS, Preset][]) {
    const hit = p.match.exec(host)
    if (!hit) continue
    const d = { ...blankDestination(kind), bucket, prefix }
    if (p.field === 'account') d.account = hit[1] ?? ''
    else d.region = hit[1] ?? ''
    return back(d)
  }
  return back({ ...blankDestination('other'), endpoint: host, region, bucket, prefix })
}

/** A strong repository password: 32 characters from a CSPRNG, URL-safe. */
export function newRepositoryPassword(): string {
  const bytes = new Uint8Array(24)
  crypto.getRandomValues(bytes)
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_')
}
