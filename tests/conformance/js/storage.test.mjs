// Storage behind /storage/v1, through supabase-js: buckets, upload, download, list, signed and
// public URLs, move, copy, remove, signed uploads, and row level security on storage.objects.
import assert from 'node:assert/strict'
import { before, describe, test } from 'node:test'
import { client, keys, kinds, makeUser, projects, signedIn, sql, suffix } from './lib.mjs'

const p = projects.a
const body = (text) => new Blob([text], { type: 'text/plain' })

for (const kind of kinds(keys.a)) {
  describe(`Storage with the ${kind.label}`, () => {
    const admin = client(p, kind.admin)
    const anon = client(p, kind.anon)
    const priv = `conf-private-${kind.tag}-${suffix}`
    const pub = `conf-public-${kind.tag}-${suffix}`
    let user, c1

    before(async () => {
      user = await makeUser(admin, `st-${kind.tag}`)
      c1 = await signedIn(p, kind.anon, user)
    })

    test('create, get and list buckets', async () => {
      const a = await admin.storage.createBucket(priv, { public: false })
      assert.ifError(a.error)
      const b = await admin.storage.createBucket(pub, { public: true })
      assert.ifError(b.error)
      const got = await admin.storage.getBucket(pub)
      assert.ifError(got.error)
      assert.equal(got.data.public, true)
      const list = await admin.storage.listBuckets()
      assert.ifError(list.error)
      const names = list.data.map((x) => x.name)
      assert.ok(names.includes(priv) && names.includes(pub), `buckets: ${names}`)
    })

    test('upload, download and list', async () => {
      const up = await admin.storage.from(priv).upload('dir/a.txt', body('hello storage'), { contentType: 'text/plain' })
      assert.ifError(up.error)
      assert.equal(up.data.path, 'dir/a.txt')
      const down = await admin.storage.from(priv).download('dir/a.txt')
      assert.ifError(down.error)
      assert.equal(await down.data.text(), 'hello storage')
      const list = await admin.storage.from(priv).list('dir')
      assert.ifError(list.error)
      assert.deepEqual(list.data.map((o) => o.name), ['a.txt'])
      assert.equal(list.data[0].metadata.mimetype, 'text/plain')
      const root = await admin.storage.from(priv).list('')
      assert.ok(root.data.some((o) => o.name === 'dir'), 'the folder is listed at the root')
    })

    test('uploading over an existing object needs upsert', async () => {
      const again = await admin.storage.from(priv).upload('dir/a.txt', body('second'))
      assert.ok(again.error, 'a second upload without upsert is refused')
      const up = await admin.storage.from(priv).upload('dir/a.txt', body('second'), { upsert: true })
      assert.ifError(up.error)
      assert.equal(await (await admin.storage.from(priv).download('dir/a.txt')).data.text(), 'second')
    })

    test('a signed URL downloads without any key', async () => {
      const { data, error } = await admin.storage.from(priv).createSignedUrl('dir/a.txt', 60)
      assert.ifError(error)
      const res = await fetch(data.signedUrl)
      assert.equal(res.status, 200)
      assert.equal(await res.text(), 'second')
      const many = await admin.storage.from(priv).createSignedUrls(['dir/a.txt'], 60)
      assert.ifError(many.error)
      assert.equal(many.data.length, 1)
      assert.ok(many.data[0].signedUrl)
    })

    test('a public bucket serves its public URL without any key; a private one does not', async () => {
      const up = await admin.storage.from(pub).upload('open.txt', body('for everyone'), { contentType: 'text/plain' })
      assert.ifError(up.error)
      const url = admin.storage.from(pub).getPublicUrl('open.txt').data.publicUrl
      const res = await fetch(url)
      assert.equal(res.status, 200)
      assert.equal(await res.text(), 'for everyone')
      const notPublic = admin.storage.from(priv).getPublicUrl('dir/a.txt').data.publicUrl
      const refused = await fetch(notPublic)
      assert.ok(refused.status === 400 || refused.status === 404, `public URL of a private object answered ${refused.status}`)
    })

    test('move, copy and remove', async () => {
      const s = admin.storage.from(priv)
      assert.ifError((await s.move('dir/a.txt', 'dir/b.txt')).error)
      assert.ifError((await s.copy('dir/b.txt', 'dir/c.txt')).error)
      const names = (await s.list('dir')).data.map((o) => o.name).sort()
      assert.deepEqual(names, ['b.txt', 'c.txt'])
      const gone = await s.download('dir/a.txt')
      assert.ok(gone.error, 'the old path no longer exists')
      const rm = await s.remove(['dir/b.txt', 'dir/c.txt'])
      assert.ifError(rm.error)
      assert.equal(rm.data.length, 2)
      assert.deepEqual((await s.list('dir')).data, [])
    })

    test('a signed upload URL takes an object without a key', async () => {
      const s = admin.storage.from(priv)
      const { data, error } = await s.createSignedUploadUrl('signed/up.txt')
      assert.ifError(error)
      assert.equal(data.path, 'signed/up.txt')
      const up = await anon.storage.from(priv).uploadToSignedUrl(data.path, data.token, body('through a token'))
      assert.ifError(up.error)
      assert.equal(await (await s.download('signed/up.txt')).data.text(), 'through a token')
    })

    test('row level security: without a policy anon and users are refused, with one a user may write', async () => {
      const denied = await anon.storage.from(priv).upload('rls/anon.txt', body('x'))
      assert.ok(denied.error, 'anon upload is refused')
      const userDenied = await c1.storage.from(priv).upload('rls/user.txt', body('x'))
      assert.ok(userDenied.error, 'a user upload without a policy is refused')
      const name = `conf_${kind.tag}_${suffix}`
      await sql(p, `
        create policy "${name}_insert" on storage.objects for insert to authenticated with check (bucket_id = '${priv}');
        create policy "${name}_select" on storage.objects for select to authenticated using (bucket_id = '${priv}');
      `)
      const ok = await c1.storage.from(priv).upload('rls/user.txt', body('user data'))
      assert.ifError(ok.error)
      const back = await c1.storage.from(priv).download('rls/user.txt')
      assert.ifError(back.error)
      assert.equal(await back.data.text(), 'user data')
      const anonRead = await anon.storage.from(priv).download('rls/user.txt')
      assert.ok(anonRead.error, 'anon still cannot read it')
      await sql(p, `drop policy "${name}_insert" on storage.objects; drop policy "${name}_select" on storage.objects;`)
    })

    test('empty and delete buckets', async () => {
      assert.ifError((await admin.storage.emptyBucket(priv)).error)
      assert.ifError((await admin.storage.deleteBucket(priv)).error)
      assert.ifError((await admin.storage.emptyBucket(pub)).error)
      assert.ifError((await admin.storage.deleteBucket(pub)).error)
      const names = (await admin.storage.listBuckets()).data.map((x) => x.name)
      assert.ok(!names.includes(priv) && !names.includes(pub))
    })
  })
}
