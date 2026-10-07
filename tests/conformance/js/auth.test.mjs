// GoTrue behind /auth/v1, through supabase-js: sign up (auto-confirm is on), sign in, refresh,
// get and update the user, sign out, and the admin API with the secret key.
import assert from 'node:assert/strict'
import { before, describe, test } from 'node:test'
import { client, jwtClaims, keys, kinds, projects, suffix } from './lib.mjs'

const p = projects.a

before(async () => {
  const r = await fetch(`${p.url}/auth/v1/settings`, { headers: { apikey: keys.a.publishable } })
  assert.equal(r.status, 200)
  const s = await r.json()
  assert.equal(s.mailer_autoconfirm, true, 'run.sh turns auto-confirm on through the Management API before the suites run')
})

for (const kind of kinds(keys.a)) {
  describe(`auth with the ${kind.label}`, () => {
    const email = `auth-${kind.tag}-${suffix}@example.com`
    const password = `Pw-${suffix}-one`
    const sb = client(p, kind.anon)
    let stale // an access token and its refresh token from before the sign-out
    let staleRefresh

    test('sign up with auto-confirm returns a confirmed user and a session', async () => {
      const { data, error } = await sb.auth.signUp({ email, password, options: { data: { plan: 'free' } } })
      assert.ifError(error)
      assert.equal(data.user.email, email)
      assert.ok(data.user.email_confirmed_at, 'the user is confirmed')
      assert.equal(data.user.user_metadata.plan, 'free')
      assert.ok(data.session?.access_token, 'a confirmed sign-up returns a session')
    })

    test('a wrong password is refused with invalid_credentials', async () => {
      const { data, error } = await client(p, kind.anon).auth.signInWithPassword({ email, password: 'not-the-password' })
      assert.ok(error)
      assert.equal(error.status, 400)
      assert.equal(error.code, 'invalid_credentials')
      assert.equal(data.session, null)
    })

    test('sign in returns a session whose token carries the project claims', async () => {
      const { data, error } = await sb.auth.signInWithPassword({ email, password })
      assert.ifError(error)
      const s = data.session
      assert.equal(s.token_type, 'bearer')
      assert.ok(s.expires_in > 0 && s.refresh_token)
      const claims = jwtClaims(s.access_token)
      assert.equal(claims.role, 'authenticated')
      assert.equal(claims.aud, 'authenticated')
      assert.equal(claims.sub, data.user.id)
      assert.equal(claims.email, email)
      const iss = new URL(claims.iss)
      assert.equal(iss.host, new URL(p.url).host, 'the issuer is the project host')
      assert.equal(iss.pathname, '/auth/v1')
    })

    test('getUser and getSession answer for the signed-in user', async () => {
      const { data, error } = await sb.auth.getUser()
      assert.ifError(error)
      assert.equal(data.user.email, email)
      const { data: s } = await sb.auth.getSession()
      assert.equal(s.session.user.email, email)
    })

    test('getClaims returns the verified claims', async () => {
      const { data, error } = await sb.auth.getClaims()
      assert.ifError(error)
      assert.equal(data.claims.email, email)
      assert.equal(data.claims.role, 'authenticated')
    })

    test('refreshSession rotates the refresh token', async () => {
      const before = (await sb.auth.getSession()).data.session
      const { data, error } = await sb.auth.refreshSession()
      assert.ifError(error)
      assert.notEqual(data.session.refresh_token, before.refresh_token)
      assert.equal(data.user.email, email)
    })

    test('updateUser changes metadata and keeps what was there', async () => {
      const { data, error } = await sb.auth.updateUser({ data: { nickname: 'conformance' } })
      assert.ifError(error)
      assert.equal(data.user.user_metadata.nickname, 'conformance')
      assert.equal(data.user.user_metadata.plan, 'free')
    })

    test('updateUser changes the password: the new one works and the old one does not', async () => {
      const next = `Pw-${suffix}-two`
      const { error } = await sb.auth.updateUser({ password: next })
      assert.ifError(error)
      const other = client(p, kind.anon)
      const ok = await other.auth.signInWithPassword({ email, password: next })
      assert.ifError(ok.error)
      const old = await client(p, kind.anon).auth.signInWithPassword({ email, password })
      assert.equal(old.error?.code, 'invalid_credentials')
    })

    test('signOut ends the session: the token and its refresh token stop working', async () => {
      const { session } = (await sb.auth.getSession()).data
      stale = session.access_token
      staleRefresh = session.refresh_token
      const { error } = await sb.auth.signOut()
      assert.ifError(error)
      assert.equal((await sb.auth.getSession()).data.session, null)
      const fresh = client(p, kind.anon)
      const user = await fresh.auth.getUser(stale)
      assert.ok(user.error, 'the access token of a signed-out session is refused')
      const refreshed = await fresh.auth.refreshSession({ refresh_token: staleRefresh })
      assert.ok(refreshed.error, 'the refresh token of a signed-out session is refused')
    })
  })

  describe(`auth admin API with the ${kind.label}`, () => {
    const admin = client(p, kind.admin)
    const email = `admin-${kind.tag}-${suffix}@example.com`
    let id

    test('createUser makes a confirmed user', async () => {
      const { data, error } = await admin.auth.admin.createUser({ email, password: `Pw-${suffix}-admin`, email_confirm: true, user_metadata: { from: 'admin' } })
      assert.ifError(error)
      id = data.user.id
      assert.ok(data.user.email_confirmed_at)
      assert.equal(data.user.user_metadata.from, 'admin')
    })

    test('listUsers includes the user', async () => {
      const { data, error } = await admin.auth.admin.listUsers({ page: 1, perPage: 1000 })
      assert.ifError(error)
      assert.ok(data.users.some((u) => u.id === id), 'the new user is listed')
    })

    test('getUserById and updateUserById', async () => {
      const got = await admin.auth.admin.getUserById(id)
      assert.ifError(got.error)
      assert.equal(got.data.user.email, email)
      const upd = await admin.auth.admin.updateUserById(id, { user_metadata: { from: 'admin', n: 2 } })
      assert.ifError(upd.error)
      assert.equal(upd.data.user.user_metadata.n, 2)
    })

    test('deleteUser removes the user', async () => {
      const del = await admin.auth.admin.deleteUser(id)
      assert.ifError(del.error)
      const got = await admin.auth.admin.getUserById(id)
      assert.ok(got.error)
      assert.equal(got.error.status, 404)
    })

    test('the low-privilege key cannot use the admin API', async () => {
      const { error } = await client(p, kind.anon).auth.admin.listUsers()
      assert.ok(error, 'listUsers with the anon-role key is refused')
      assert.ok([401, 403].includes(error.status), `status ${error.status}`)
    })
  })
}
