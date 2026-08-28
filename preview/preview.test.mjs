import assert from 'node:assert/strict';
import test from 'node:test';
import {
  DEFAULTS,
  buildAwsArgs,
  buildBootstrapUserData,
  buildDnsUpsertChanges,
  buildGoldenAmiFilters,
  buildHostname,
  buildRunInstancesArgs,
  buildSetupRolePolicy,
  buildSshArgs,
  buildTags,
  countManagedInstancesForCreator,
  creatorIdFromIdentity,
  isManagedResource,
  normalizeDnsLabel,
  normalizeOwner,
  ownerCandidatesFromIdentity,
  parseDuration,
  publicKeyFromIdentity,
  tagsToAws,
  usesStandardOnDemandQuota,
  validateCpuInstanceType,
  validateGoldenAmi,
} from './lib.mjs';
import {
  assertDnsUnitAvailable,
  assertSetupResourceOwned,
  ownedDnsRecords,
  parseCliArgs,
  terminatePreview,
} from './preview.mjs';

test('normalizes DNS labels and email owners', () => {
  assert.equal(normalizeDnsLabel(' DUR-5542 / AI Preview '), 'dur-5542-ai-preview');
  assert.equal(normalizeOwner('Vitalii.Shein+preview@example.com'), 'vitalii-shein-preview');
  assert.equal(normalizeDnsLabel('Crème brûlée'), 'creme-brulee');
});

test('long normalized labels are stable and within DNS limits', () => {
  const value = normalizeDnsLabel('A'.repeat(90));
  assert.equal(value.length, 63);
  assert.match(value, /^a+-[a-f0-9]{8}$/);
  assert.equal(value, normalizeDnsLabel('A'.repeat(90)));
});

test('parses compound durations and rejects incomplete input', () => {
  assert.equal(parseDuration('2d'), 172800);
  assert.equal(parseDuration('1d12h30m'), 131400);
  assert.throws(() => parseDuration('48'), /Invalid duration/);
  assert.throws(() => parseDuration('1h later'), /Invalid duration/);
  assert.throws(() => parseDuration('0h'), /Invalid duration/);
});

test('builds normalized hostname and managed tags', () => {
  const hostname = buildHostname('DUR-5542', 'Vitalii@example.com', DEFAULTS.domain);
  assert.equal(hostname, 'dur-5542.vitalii.duranta-preview.com');
  const tags = buildTags({
    issue: 'DUR-5542',
    owner: 'Vitalii@example.com',
    hostname,
    expiresAt: '2026-08-30T00:00:00.000Z',
  });
  assert.deepEqual(tags, {
    Name: hostname,
    ManagedBy: 'duranta-preview',
    Issue: 'dur-5542',
    Owner: 'vitalii',
    Hostname: hostname,
    ExpiresAt: '2026-08-30T00:00:00.000Z',
  });
  assert.equal(isManagedResource(tagsToAws(tags)), true);
  assert.equal(isManagedResource([{ Key: 'ManagedBy', Value: 'someone-else' }]), false);
});

test('constructs AWS CLI arguments without a shell', () => {
  assert.deepEqual(
    buildAwsArgs({ profile: 'preview', region: 'us-west-2' }, ['ec2', 'describe-instances']),
    ['--profile', 'preview', '--region', 'us-west-2', '--no-cli-pager', '--output', 'json', 'ec2', 'describe-instances'],
  );
});

test('recognizes Standard On-Demand instance families', () => {
  assert.equal(usesStandardOnDemandQuota('m7i.4xlarge'), true);
  assert.equal(usesStandardOnDemandQuota('c7i.2xlarge'), true);
  assert.equal(usesStandardOnDemandQuota('g5.4xlarge'), false);
});

test('counts new instances by stable AWS creator and legacy instances by identity owner', () => {
  const identity = {
    UserId: 'AROAEXAMPLE:vitalii@example.com',
    Arn: 'arn:aws:sts::123456789012:assumed-role/Preview/vitalii@example.com',
  };
  const instance = (tags) => ({ Tags: tagsToAws({ ManagedBy: 'duranta-preview', ...tags }) });
  const instances = [
    instance({ CreatorId: identity.UserId, Owner: 'anything' }),
    instance({ CreatorId: 'AROAOTHER:alex@example.com', Owner: 'vitalii' }),
    instance({ Owner: 'vitalii' }),
    instance({ Owner: 'some-custom-alias' }),
  ];
  assert.equal(creatorIdFromIdentity(identity), identity.UserId);
  assert.deepEqual(ownerCandidatesFromIdentity(identity), ['vitalii']);
  assert.equal(countManagedInstancesForCreator(
    instances,
    creatorIdFromIdentity(identity),
    ownerCandidatesFromIdentity(identity),
  ), 2);
});

test('validates CPU-only x86_64 Standard instance types from AWS metadata', () => {
  const cpu = {
    InstanceType: 'm7i.4xlarge',
    ProcessorInfo: { SupportedArchitectures: ['x86_64'] },
    VCpuInfo: { DefaultVCpus: 16 },
  };
  assert.equal(validateCpuInstanceType('m7i.4xlarge', cpu), 16);
  assert.throws(() => validateCpuInstanceType('g5.4xlarge', {
    ...cpu,
    InstanceType: 'g5.4xlarge',
    GpuInfo: { Gpus: [{ Name: 'A10G' }] },
  }), /CPU-only/);
  assert.throws(() => validateCpuInstanceType('m7i.4xlarge', {
    ...cpu,
    GpuInfo: { Gpus: [{ Name: 'unexpected' }] },
  }), /CPU-only/);
  assert.throws(() => validateCpuInstanceType('c7g.4xlarge', {
    ...cpu,
    InstanceType: 'c7g.4xlarge',
    ProcessorInfo: { SupportedArchitectures: ['arm64'] },
  }), /x86_64/);
  assert.throws(() => validateCpuInstanceType('p5.48xlarge', {
    ...cpu,
    InstanceType: 'p5.48xlarge',
  }), /not in the supported EC2 Standard/);
});

test('run-instances arguments enforce disposable and hardened settings', () => {
  const args = buildRunInstancesArgs({
    amiId: 'ami-123',
    clientToken: 'token',
    instanceProfile: 'duranta-preview-instance',
    instanceType: 'm7i.4xlarge',
    rootDeviceName: '/dev/sda1',
    securityGroupId: 'sg-123',
    subnetId: 'subnet-123',
    tags: {
      Name: 'dur-5542.vitalii.duranta-preview.com',
      ManagedBy: 'duranta-preview',
      Issue: 'dur-5542',
      Owner: 'vitalii',
      Hostname: 'dur-5542.vitalii.duranta-preview.com',
      ExpiresAt: '2026-08-30T00:00:00.000Z',
    },
    userData: '#!/bin/sh',
    volumeSize: 200,
  });
  assert.equal(args[0], 'ec2');
  assert.equal(args[1], 'run-instances');
  assert.equal(args[args.indexOf('--instance-initiated-shutdown-behavior') + 1], 'terminate');
  const metadata = JSON.parse(args[args.indexOf('--metadata-options') + 1]);
  assert.equal(metadata.HttpTokens, 'required');
  assert.equal(metadata.HttpPutResponseHopLimit, 1);
  assert.ok(args.includes('--disable-api-stop'));
  const network = JSON.parse(args[args.indexOf('--network-interfaces') + 1]);
  assert.equal(network[0].AssociatePublicIpAddress, true);
  assert.equal(network[0].DeleteOnTermination, true);
  const block = JSON.parse(args[args.indexOf('--block-device-mappings') + 1]);
  assert.deepEqual(block[0].Ebs, {
    DeleteOnTermination: true,
    Encrypted: true,
    VolumeSize: 200,
    VolumeType: 'gp3',
  });
});

test('DNS change owns app, wildcard, and marker record sets', () => {
  const changes = buildDnsUpsertChanges({
    hostname: 'dur-5542.vitalii.duranta-preview.com',
    instanceId: 'i-123',
    owner: 'vitalii',
    publicIp: '203.0.113.10',
  });
  assert.deepEqual(changes.map((change) => [change.ResourceRecordSet.Name, change.ResourceRecordSet.Type]), [
    ['dur-5542.vitalii.duranta-preview.com', 'A'],
    ['*.dur-5542.vitalii.duranta-preview.com', 'A'],
    ['_duranta-preview.dur-5542.vitalii.duranta-preview.com', 'TXT'],
  ]);
  const marker = JSON.parse(JSON.parse(changes[2].ResourceRecordSet.ResourceRecords[0].Value));
  assert.deepEqual(marker, { managedBy: 'duranta-preview', instanceId: 'i-123', owner: 'vitalii' });
});

test('bootstrap receives exact cleanup and TTL context', () => {
  const userData = buildBootstrapUserData({
    hostname: 'dur-5542.vitalii.duranta-preview.com',
    hostedZoneId: '/hostedzone/Z123',
    issue: 'DUR-5542',
    owner: 'Vitalii',
    expiresAt: '2026-08-30T00:00:00.000Z',
  });
  assert.equal(userData, [
    '#!/bin/sh',
    'set -u',
    '/usr/local/bin/duranta-preview-bootstrap \\',
    '  --hostname dur-5542.vitalii.duranta-preview.com \\',
    '  --issue dur-5542 \\',
    '  --owner vitalii \\',
    '  --hosted-zone-id Z123 \\',
    '  --expires-at 2026-08-30T00:00:00.000Z',
    'status=$?',
    'if [ "$status" -eq 0 ]; then',
    '  exit 0',
    'fi',
    'message="duranta-preview bootstrap failed with status $status; requesting instance termination"',
    'printf "%s\\n" "$message" >&2',
    'if command -v logger >/dev/null 2>&1; then',
    '  logger -t duranta-preview "$message"',
    'fi',
    'sync',
    'if command -v shutdown >/dev/null 2>&1; then',
    '  shutdown -h now',
    'else',
    '  systemctl poweroff --no-block',
    'fi',
    'exit "$status"',
    '',
  ].join('\n'));
});

test('golden AMI discovery matches bake tags', () => {
  assert.deepEqual(buildGoldenAmiFilters(), [
    { Name: 'state', Values: ['available'] },
    { Name: 'tag:ManagedBy', Values: ['duranta-preview'] },
    { Name: 'tag:Purpose', Values: ['golden'] },
  ]);
});

test('golden AMI must be owned, tagged, x86_64, and contain only its disposable root EBS', () => {
  const image = {
    ImageId: 'ami-123',
    State: 'available',
    OwnerId: '123456789012',
    Architecture: 'x86_64',
    RootDeviceType: 'ebs',
    RootDeviceName: '/dev/xvda',
    Tags: tagsToAws({ ManagedBy: 'duranta-preview', Purpose: 'golden' }),
    BlockDeviceMappings: [{
      DeviceName: '/dev/xvda',
      Ebs: { DeleteOnTermination: true, SnapshotId: 'snap-root' },
    }],
  };
  assert.equal(validateGoldenAmi(image, '123456789012'), image);
  assert.throws(() => validateGoldenAmi({ ...image, OwnerId: '210987654321' }, '123456789012'), /not owned/);
  assert.throws(() => validateGoldenAmi({ ...image, Tags: [] }, '123456789012'), /not tagged/);
  assert.throws(() => validateGoldenAmi({
    ...image,
    BlockDeviceMappings: [
      ...image.BlockDeviceMappings,
      { DeviceName: '/dev/sdf', Ebs: { DeleteOnTermination: false, SnapshotId: 'snap-extra' } },
    ],
  }, '123456789012'), /extra EBS/);
});

test('DNS create changes use CREATE to make a concurrent record fail instead of overwrite', () => {
  const changes = buildDnsUpsertChanges({
    action: 'CREATE',
    hostname: 'dur-5542.vitalii.duranta-preview.com',
    instanceId: 'i-123',
    owner: 'vitalii',
    publicIp: '203.0.113.10',
  });
  assert.deepEqual(changes.map((change) => change.Action), ['CREATE', 'CREATE', 'CREATE']);
});

test('DNS preflight accepts an empty unit and refuses existing managed or malformed units', () => {
  const hostname = 'dur-5542.vitalii.duranta-preview.com';
  assert.doesNotThrow(() => assertDnsUnitAvailable([], hostname, 'vitalii'));
  const managed = buildDnsUpsertChanges({
    hostname,
    instanceId: 'i-123',
    owner: 'vitalii',
    publicIp: '203.0.113.10',
  }).map((change) => change.ResourceRecordSet);
  assert.throws(() => assertDnsUnitAvailable(managed, hostname, 'vitalii'), /Managed DNS already exists/);
  assert.throws(() => assertDnsUnitAvailable(managed.slice(0, 2), hostname, 'vitalii'), /unmanaged or malformed/);
  assert.throws(() => assertDnsUnitAvailable([{
    Name: hostname,
    Type: 'CNAME',
    TTL: 60,
    ResourceRecords: [{ Value: 'somewhere.example.com' }],
  }], hostname, 'vitalii'), /unmanaged or malformed/);
});

test('setup refuses to adopt untagged IAM resources', () => {
  assert.throws(() => assertSetupResourceOwned({ Tags: [] }, 'IAM role preview'), /untagged IAM role/);
  assert.doesNotThrow(() => assertSetupResourceOwned({
    Tags: [{ Key: 'ManagedBy', Value: 'duranta-preview' }],
  }, 'IAM role preview'));
});

test('SSH uses SSM, Instance Connect target, and agent forwarding', () => {
  const args = buildSshArgs(
    { profile: 'preview', region: 'us-west-2', sshUser: 'ec2-user' },
    { InstanceId: 'i-123' },
    '/tmp/key',
  );
  assert.equal(args[0], '-A');
  assert.ok(args.some((value) => value.includes('ssm start-session')));
  assert.ok(args.includes('ec2-user@i-123'));
  assert.ok(args.includes('/tmp/key'));
});

test('selects one agent public key so SSH can constrain offered identities', () => {
  const first = 'ssh-ed25519 AAAAfirst developer@example.com';
  const selected = publicKeyFromIdentity(null, () => ({
    status: 0,
    stdout: `${first}\nssh-ed25519 AAAAsecond another@example.com\n`,
  }));
  assert.deepEqual(selected, { publicKey: first, identityFile: null });
});

test('DNS deletion requires a valid marker and matching public IP', () => {
  const hostname = 'dur-5542.vitalii.duranta-preview.com';
  const instance = {
    InstanceId: 'i-123',
    PublicIpAddress: '203.0.113.10',
    Tags: tagsToAws({
      ManagedBy: 'duranta-preview',
      Owner: 'vitalii',
      Hostname: hostname,
    }),
  };
  const upserts = buildDnsUpsertChanges({
    hostname,
    instanceId: instance.InstanceId,
    owner: 'vitalii',
    publicIp: instance.PublicIpAddress,
  });
  const records = upserts.map((change) => change.ResourceRecordSet);
  assert.equal(ownedDnsRecords(records, instance).records.length, 3);

  const withoutMarker = ownedDnsRecords(records.slice(0, 2), instance);
  assert.equal(withoutMarker.records.length, 0);
  assert.match(withoutMarker.warning, /marker is missing or invalid/);

  const mismatched = structuredClone(records);
  mismatched[0].ResourceRecords[0].Value = '203.0.113.99';
  const mismatchPlan = ownedDnsRecords(mismatched, instance);
  assert.equal(mismatchPlan.records.length, 0);
  assert.match(mismatchPlan.warning, /no longer points/);
});

test('termination proceeds when the DNS marker is missing', async () => {
  const hostname = 'dur-5542.vitalii.duranta-preview.com';
  const instance = {
    InstanceId: 'i-123',
    InstanceType: 'm7i.4xlarge',
    PublicIpAddress: '203.0.113.10',
    State: { Name: 'running' },
    Tags: tagsToAws({
      Name: hostname,
      ManagedBy: 'duranta-preview',
      Issue: 'dur-5542',
      Owner: 'vitalii',
      Hostname: hostname,
    }),
  };
  const calls = [];
  const aws = {
    run(args) {
      calls.push(args);
      if (args[0] === 'ec2' && args[1] === 'describe-instances') {
        return { Reservations: [{ Instances: [instance] }] };
      }
      if (args[0] === 'route53' && args[1] === 'get-hosted-zone') {
        return { HostedZone: { Name: 'duranta-preview.com.', Config: { PrivateZone: false } } };
      }
      if (args[0] === 'route53' && args[1] === 'list-resource-record-sets') {
        return {
          ResourceRecordSets: buildDnsUpsertChanges({
            hostname,
            instanceId: instance.InstanceId,
            owner: 'vitalii',
            publicIp: instance.PublicIpAddress,
          }).slice(0, 2).map((change) => change.ResourceRecordSet),
        };
      }
      return {};
    },
  };
  const originalLog = console.log;
  const originalWarn = console.warn;
  console.log = () => {};
  console.warn = () => {};
  try {
    await terminatePreview(aws, {
      domain: 'duranta-preview.com',
      profile: 'preview',
      region: 'us-west-2',
    }, instance.InstanceId, { yes: true, hostedZoneId: 'Z123' });
  } finally {
    console.log = originalLog;
    console.warn = originalWarn;
  }
  const enableStop = calls.findIndex((args) => args[0] === 'ec2' && args[1] === 'modify-instance-attribute');
  const terminate = calls.findIndex((args) => args[0] === 'ec2' && args[1] === 'terminate-instances');
  assert.ok(enableStop >= 0);
  assert.ok(calls[enableStop].includes('--no-disable-api-stop'));
  assert.ok(terminate > enableStop);
  assert.equal(calls.some((args) => args[0] === 'route53' && args[1] === 'change-resource-record-sets'), false);
});

test('setup role policy scopes DNS writes to the hosted zone and record types', () => {
  const policy = buildSetupRolePolicy('/hostedzone/Z123', 'duranta-preview.com');
  const change = policy.Statement.find((statement) => statement.Action === 'route53:ChangeResourceRecordSets');
  assert.equal(change.Resource, 'arn:aws:route53:::hostedzone/Z123');
  assert.deepEqual(change.Condition['ForAllValues:StringEquals']['route53:ChangeResourceRecordSetsRecordTypes'], ['A', 'TXT']);
});

test('parses command options without evaluating values', () => {
  assert.deepEqual(parseCliArgs([
    'create', 'DUR-5542', '--owner=vitalii', '--ttl', '2d', '--profile', 'preview',
  ]), {
    positionals: ['create', 'DUR-5542'],
    options: { owner: 'vitalii', ttl: '2d', profile: 'preview' },
  });
  assert.throws(() => parseCliArgs(['list', '--wat']), /Unknown option/);
});
