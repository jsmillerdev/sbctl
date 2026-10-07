// JWT checks for functions deployed with verify_jwt (the default).
//
// Projects sign their keys with one HS256 secret, so that is the only algorithm the
// main service verifies; it uses WebCrypto and no remote module, so the main service
// starts without network access. Error codes and the response shape follow the
// self-hosted main service of supabase/supabase (docker/volumes/functions/main/index.ts).

export const ErrorCodes = {
  InvalidLegacyJWT: 'UNAUTHORIZED_LEGACY_JWT',
  InvalidTokenFormat: 'UNAUTHORIZED_INVALID_JWT_FORMAT',
  UnsupportedTokenAlgorithm: 'UNAUTHORIZED_UNSUPPORTED_TOKEN_ALGORITHM',
  MissingAuthHeader: 'UNAUTHORIZED_NO_AUTH_HEADER',
  InvalidAsymmetricJWT: 'UNAUTHORIZED_ASYMMETRIC_JWT',
  NotFound: 'NOT_FOUND',
  BootError: 'BOOT_ERROR',
  EdgeFunctionError: 'EDGE_FUNCTION_ERROR',
  IdleTimeout: 'IDLE_TIMEOUT',
  WorkerResourceLimit: 'WORKER_RESOURCE_LIMIT',
  WorkerError: 'WORKER_ERROR',
  InvalidResponseStatusCode: 'INVALID_RESPONSE_STATUS_CODE',
  BadRequest: 'BAD_REQUEST',
} as const

export type ErrorCode = typeof ErrorCodes[keyof typeof ErrorCodes]

export interface AuthFailure {
  code: ErrorCode
  message: string
}

/**
 * The token a request carries: the Authorization bearer, or the sb-api-key header the
 * proxy sets when the caller sent an opaque sb_publishable_ or sb_secret_ key (the
 * proxy has replaced it with the matching JWT there).
 */
export function extractToken(req: Request): string | AuthFailure {
  const authorization = req.headers.get('authorization')
  const compat = req.headers.get('sb-api-key')
  if (!authorization && !compat) {
    return { code: ErrorCodes.MissingAuthHeader, message: 'Missing authorization header' }
  }
  const parts = (authorization ?? '').trim().split(/\s+/)
  const bearer = parts.length === 2 && parts[0].toLowerCase() === 'bearer' ? parts[1] : null
  const token = !bearer || bearer.startsWith('sb_')
    ? compat?.replace(/^Bearer\s+/i, '').trim()
    : bearer
  if (!token) return { code: ErrorCodes.InvalidTokenFormat, message: 'Invalid JWT format' }
  return token
}

function b64urlToBytes(s: string): Uint8Array<ArrayBuffer> | null {
  if (!/^[A-Za-z0-9_-]*$/.test(s)) return null
  const pad = '='.repeat((4 - (s.length % 4)) % 4)
  try {
    const bin = atob(s.replace(/-/g, '+').replace(/_/g, '/') + pad)
    const out = new Uint8Array(bin.length)
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
    return out
  } catch {
    return null
  }
}

function parseJSON(bytes: Uint8Array): Record<string, unknown> | null {
  try {
    const v = JSON.parse(new TextDecoder().decode(bytes))
    return v && typeof v === 'object' && !Array.isArray(v) ? v : null
  } catch {
    return null
  }
}

const keyCache = new Map<string, Promise<CryptoKey>>()

function hmacKey(secret: string): Promise<CryptoKey> {
  let k = keyCache.get(secret)
  if (!k) {
    // The cache is keyed by the secret itself and bounded: rotation replaces a key, and
    // the main service holds at most a few hundred projects' secrets in memory anyway.
    if (keyCache.size >= 1024) keyCache.clear()
    k = crypto.subtle.importKey(
      'raw',
      new TextEncoder().encode(secret),
      { name: 'HMAC', hash: 'SHA-256' },
      false,
      ['verify'],
    )
    keyCache.set(secret, k)
  }
  return k
}

/**
 * Checks an HS256 JWT against secret: signature (constant time, by WebCrypto), exp and
 * nbf. It returns null when the token is valid.
 */
export async function verifyJWT(
  token: string,
  secret: string,
  nowMs: number = Date.now(),
): Promise<AuthFailure | null> {
  const format: AuthFailure = { code: ErrorCodes.InvalidTokenFormat, message: 'Invalid JWT format' }
  const parts = token.split('.')
  if (parts.length !== 3) return format
  const header = b64urlToBytes(parts[0])
  const payload = b64urlToBytes(parts[1])
  const signature = b64urlToBytes(parts[2])
  if (!header || !payload || !signature) return format
  const h = parseJSON(header)
  const claims = parseJSON(payload)
  if (!h || !claims || typeof h.alg !== 'string') return format
  if (h.alg !== 'HS256') {
    if (h.alg === 'ES256' || h.alg === 'RS256') {
      return { code: ErrorCodes.InvalidAsymmetricJWT, message: 'Invalid JWT' }
    }
    return {
      code: ErrorCodes.UnsupportedTokenAlgorithm,
      message: `Unsupported JWT algorithm ${h.alg}`,
    }
  }
  const invalid: AuthFailure = { code: ErrorCodes.InvalidLegacyJWT, message: 'Invalid JWT' }
  if (!secret) return invalid
  const signed = new TextEncoder().encode(`${parts[0]}.${parts[1]}`)
  let ok = false
  try {
    ok = await crypto.subtle.verify('HMAC', await hmacKey(secret), signature, signed)
  } catch {
    ok = false
  }
  if (!ok) return invalid
  const now = Math.floor(nowMs / 1000)
  if (claims.exp !== undefined && (typeof claims.exp !== 'number' || claims.exp <= now)) {
    return invalid
  }
  if (claims.nbf !== undefined && (typeof claims.nbf !== 'number' || claims.nbf > now)) {
    return invalid
  }
  return null
}

/** The 401 response of a failed check, shaped like the self-hosted main service's. */
export function authErrorResponse(f: AuthFailure): Response {
  return Response.json(
    { code: f.code, message: f.message, msg: f.message },
    {
      status: 401,
      headers: { 'sb-error-code': f.code, 'Access-Control-Expose-Headers': 'sb-error-code' },
    },
  )
}
