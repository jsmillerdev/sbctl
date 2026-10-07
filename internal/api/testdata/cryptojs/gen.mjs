// Generates crypto-js AES vectors for pgmeta_crypto_test.go. Run:
//   npm install --no-save crypto-js@4 && node gen.mjs > vectors.json
// crypto-js is what postgres-meta (src/server/routes/index.ts) uses to decrypt the
// x-connection-encrypted header: CryptoJS.AES.decrypt(header, CRYPTO_KEY).
import CryptoJS from 'crypto-js'

const cases = [
  ['SAMPLE_KEY', 'postgres://postgres:secret@127.0.0.1:20000/postgres'],
  ['k3y with spaces & symbols/+=', 'postgresql://supabase_admin:p%40ss%2Fw0rd@127.0.0.1:20003/postgres?sslmode=disable'],
  ['0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', 'postgres://postgres:ünïcödé-日本語@127.0.0.1:5433/postgres'],
]
const out = cases.map(([key, plaintext]) => ({
  key,
  plaintext,
  encrypted: CryptoJS.AES.encrypt(plaintext, key).toString(),
}))
// Round trip through the real library so a bad vector fails here, not in Go.
for (const c of out) {
  const back = CryptoJS.AES.decrypt(c.encrypted, c.key).toString(CryptoJS.enc.Utf8)
  if (back !== c.plaintext) throw new Error('self-check failed')
}
console.log(JSON.stringify(out, null, 2))
