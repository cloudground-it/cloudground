/**
 * Passkeys in the browser: the panel sends WebAuthn options as JSON with
 * base64url fields ({ publicKey: … }), and takes the credential back as JSON
 * the same way. Modern browsers convert both themselves
 * (PublicKeyCredential.parse…FromJSON, credential.toJSON); the fallback here
 * does it by hand for the ones that do not yet.
 */

type Json = Record<string, any>

function fromB64url(s: string): ArrayBuffer {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/').padEnd(Math.ceil(s.length / 4) * 4, '=')
  const bin = atob(b64)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out.buffer
}

function toB64url(buf: ArrayBuffer | ArrayBufferView | null | undefined): string | undefined {
  if (!buf) return undefined
  const bytes = buf instanceof ArrayBuffer ? new Uint8Array(buf) : new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength)
  let bin = ''
  for (const b of bytes) bin += String.fromCharCode(b)
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

/** Whether this browser can use passkeys here (a secure context with WebAuthn). */
export function passkeysSupported(): boolean {
  return typeof window !== 'undefined' && window.isSecureContext && 'PublicKeyCredential' in window && !!navigator.credentials
}

const PKC = () => (window as any).PublicKeyCredential

function creationOptions(o: Json): CredentialCreationOptions {
  const pk = o.publicKey ?? o
  if (typeof PKC().parseCreationOptionsFromJSON === 'function') return { publicKey: PKC().parseCreationOptionsFromJSON(pk) }
  return {
    publicKey: {
      ...pk,
      challenge: fromB64url(pk.challenge),
      user: { ...pk.user, id: fromB64url(pk.user.id) },
      excludeCredentials: (pk.excludeCredentials ?? []).map((c: Json) => ({ ...c, id: fromB64url(c.id) }))
    }
  }
}

function requestOptions(o: Json): CredentialRequestOptions {
  const pk = o.publicKey ?? o
  if (typeof PKC().parseRequestOptionsFromJSON === 'function') return { publicKey: PKC().parseRequestOptionsFromJSON(pk) }
  return {
    publicKey: {
      ...pk,
      challenge: fromB64url(pk.challenge),
      allowCredentials: (pk.allowCredentials ?? []).map((c: Json) => ({ ...c, id: fromB64url(c.id) }))
    }
  }
}

function credentialJSON(cred: any): Json {
  if (typeof cred.toJSON === 'function') return cred.toJSON()
  const r = cred.response
  return {
    id: cred.id,
    rawId: toB64url(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    clientExtensionResults: cred.getClientExtensionResults?.() ?? {},
    response: r.attestationObject
      ? { clientDataJSON: toB64url(r.clientDataJSON), attestationObject: toB64url(r.attestationObject), transports: r.getTransports?.() ?? [] }
      : { clientDataJSON: toB64url(r.clientDataJSON), authenticatorData: toB64url(r.authenticatorData), signature: toB64url(r.signature), userHandle: toB64url(r.userHandle) }
  }
}

/** Makes a passkey from the panel's creation options; the credential as JSON. */
export async function createPasskey(options: Json): Promise<Json> {
  const cred = await navigator.credentials.create(creationOptions(options))
  if (!cred) throw new Error('no passkey')
  return credentialJSON(cred)
}

/** Answers the panel's request options with a passkey; the assertion as JSON. */
export async function getPasskey(options: Json): Promise<Json> {
  const cred = await navigator.credentials.get(requestOptions(options))
  if (!cred) throw new Error('no passkey')
  return credentialJSON(cred)
}

/** A passkey prompt the person closed or let time out, which needs no error toast. */
export function passkeyCancelled(e: unknown): boolean {
  return e instanceof DOMException && (e.name === 'NotAllowedError' || e.name === 'AbortError')
}

/** A device name to suggest for a new passkey, from the browser and system. */
export function suggestedPasskeyName(): string {
  const ua = navigator.userAgent
  const os = /iPhone|iPad/.test(ua) ? 'iOS' : /Android/.test(ua) ? 'Android' : /Mac OS X/.test(ua) ? 'macOS' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : ''
  const browser = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : ''
  return [browser, os].filter(Boolean).join(' · ') || 'Passkey'
}
