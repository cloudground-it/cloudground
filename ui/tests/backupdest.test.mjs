// The guided backup destination on the Settings page: every preset makes
// the repository string the API already takes, and a stored repository
// reads back to the same string (or is kept as typed). Run with `npm test`.
import { test } from 'node:test'
import assert from 'node:assert/strict'

const { blankDestination, destinationOf, newRepositoryPassword, regionOf, repositoryKind, repositoryOf } = await import('../app/utils/backupDestination.ts')

const d = (kind, fields) => ({ ...blankDestination(kind), ...fields })

test('each preset makes an s3:https:// repository on its endpoint', () => {
  assert.equal(repositoryOf(d('aws', { region: 'eu-central-1', bucket: 'b' })), 's3:https://s3.eu-central-1.amazonaws.com/b')
  assert.equal(repositoryOf(d('r2', { account: 'abc123', bucket: 'b', prefix: 'cloudground' })), 's3:https://abc123.r2.cloudflarestorage.com/b/cloudground')
  assert.equal(repositoryOf(d('b2', { region: 'eu-central-003', bucket: 'b' })), 's3:https://s3.eu-central-003.backblazeb2.com/b')
  assert.equal(repositoryOf(d('wasabi', { region: 'us-east-1', bucket: 'b' })), 's3:https://s3.us-east-1.wasabisys.com/b')
  assert.equal(repositoryOf(d('hetzner', { region: 'fsn1', bucket: 'b' })), 's3:https://fsn1.your-objectstorage.com/b')
  assert.equal(repositoryOf(d('ovh', { region: 'gra', bucket: 'b' })), 's3:https://s3.gra.io.cloud.ovh.net/b')
  assert.equal(repositoryOf(d('other', { endpoint: 'https://minio.example.com/', bucket: '/b/', prefix: '/x/y/' })), 's3:https://minio.example.com/b/x/y')
  assert.equal(repositoryOf(d('local', { path: '/var/backups/cg/' })), '/var/backups/cg')
})

test('a destination with a required field empty makes no repository', () => {
  assert.equal(repositoryOf(d('aws', { bucket: 'b' })), '')
  assert.equal(repositoryOf(d('r2', { account: 'abc' })), '')
  assert.equal(repositoryOf(d('local', { path: 'relative/path' })), '')
})

test('the region sent with the repository', () => {
  assert.equal(regionOf(d('r2', { account: 'a' })), 'auto')
  assert.equal(regionOf(d('aws', { region: 'eu-west-1' })), 'eu-west-1')
  assert.equal(regionOf(d('other', { region: '' })), '')
  assert.equal(regionOf(d('local', { path: '/x' })), '')
})

test('a stored repository reads back to the same string', () => {
  for (const repo of [
    's3:https://s3.eu-central-1.amazonaws.com/b',
    's3:https://abc123.r2.cloudflarestorage.com/b/cloudground',
    's3:https://fsn1.your-objectstorage.com/b/a/b',
    's3:https://minio.example.com:9000/b',
    '/var/backups/cg'
  ]) {
    const back = destinationOf(repo)
    assert.notEqual(back.kind, 'advanced', repo)
    assert.equal(repositoryOf(back), repo)
  }
  assert.equal(destinationOf('s3:https://abc123.r2.cloudflarestorage.com/b').kind, 'r2')
  assert.equal(destinationOf('s3:https://minio.example.com/b', 'us-east-1').region, 'us-east-1')
})

test('what the presets cannot make is kept as typed', () => {
  for (const repo of [
    'sftp:backup@host:/srv/restic',
    'rest:https://user:••••••••@host/',
    's3:https://key:••••••••@s3.amazonaws.com/b',
    's3:s3.amazonaws.com/b',
    'b2:bucket:path'
  ]) {
    const back = destinationOf(repo)
    assert.equal(back.kind, 'advanced', repo)
    assert.equal(repositoryOf(back), repo)
  }
})

test('a generated repository password is long and URL-safe', () => {
  const a = newRepositoryPassword()
  assert.equal(a.length, 32)
  assert.match(a, /^[A-Za-z0-9_-]+$/)
  assert.notEqual(a, newRepositoryPassword())
})

test('the move dialog asks for what the new repository needs', () => {
  assert.equal(repositoryKind('s3:https://s3.eu-central-1.amazonaws.com/b'), 's3')
  assert.equal(repositoryKind(' b2:bucket:path '), 'b2')
  assert.equal(repositoryKind('sftp:user@host:/srv/restic'), 'sftp')
  assert.equal(repositoryKind('sftp://user@host:2222//srv/restic'), 'sftp')
  assert.equal(repositoryKind('/mnt/backups'), '')
  assert.equal(repositoryKind('rest:https://host/'), '')
})
