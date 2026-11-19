// Runs with the immutable worker Node before the project's install command.
const fs = require('node:fs');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { createHash } = require('node:crypto');

const project = JSON.parse(fs.readFileSync('package.json'));
const manager = process.argv[2];
const tools = path.resolve('.toolchain');
const managers = path.join(process.env.HOME, '.cache/holon-manager');
const run = (cmd, args, options) => execFileSync(cmd, args, { stdio: 'inherit', ...options });

function versionFile(name) {
  try { return fs.readFileSync(name, 'utf8').trim(); }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
}

function selectNode() {
  const declared = versionFile('.nvmrc') || versionFile('.node-version') || project.engines?.node;
  const engines = [project.devEngines?.runtime].flat().filter(entry => entry?.name === 'node');
  const range = engines.every(entry => entry.version) && engines.map(entry => entry.version).join(' || ');
  const fnm = process.env.FNM_DIR;
  const cache = '/opt/holon/node/versions';
  const versions = fs.readdirSync(cache);
  const exact = 'v' + (declared || range || '').replace(/^v/, '');
  fs.mkdirSync(path.join(fnm, 'aliases'), { recursive: true });
  fs.symlinkSync('aliases/default/bin', path.join(fnm, 'bin'));
  if (versions.includes(exact)) {
    return fs.symlinkSync(path.join(cache, exact, 'installation'), path.join(fnm, 'aliases/default'));
  }
  const installed = path.join(fnm, 'node-versions');
  fs.mkdirSync(installed);
  for (const version of versions) fs.symlinkSync(path.join(cache, version), path.join(installed, version));
  const install = ['install', '--progress', 'never'];
  if (declared || !range) return run('fnm', declared ? install : [...install, '--lts']);
  // fnm parses npm ranges from engines.node. npm applies devEngines.onFail.
  fs.writeFileSync(path.join(managers, 'package.json'), JSON.stringify({ engines: { node: range } }));
  try {
    execFileSync('fnm', install, { cwd: managers, stdio: ['ignore', 'inherit', 'pipe'] });
  } catch (error) {
    if (!/^error: Can't find (version in dotfiles\.|a Node version that matches )/m.test(String(error.stderr))) throw error;
    run('fnm', [...install, '--lts']);
  }
}

function selectManager() {
  const spec = project.packageManager || manager;
  const pin = /^(npm|pnpm)(?:@(\d+\.\d+\.\d+(?:-[\w.-]+)?)(?:\+(sha224|sha256|sha384|sha512)\.([\da-f]+))?)?$/.exec(spec);
  if (!pin || pin[1] !== manager) throw new Error('invalid packageManager declaration: ' + spec);
  if (manager === 'npm' && !pin[3]) {
    const bundled = path.join(tools, 'node/aliases/default/lib/node_modules/npm/package.json');
    if (!pin[2] || JSON.parse(fs.readFileSync(bundled)).version === pin[2]) return;
  }
  if (manager === 'pnpm') {
    const config = path.join(process.env.HOME, '.config/pnpm');
    fs.mkdirSync(config, { recursive: true });
    try { fs.writeFileSync(path.join(config, 'config.yaml'), 'nodeDownloadMirrors:\n  release: https://nodejs.org/dist/\n', { flag: 'wx' }); }
    catch (error) { if (error.code !== 'EEXIST') throw error; }
  }
  if (pin[2] && !pin[3]) {
    const cached = '/usr/local/lib/node_modules/' + manager + '/package.json';
    if (JSON.parse(fs.readFileSync(cached)).version === pin[2]) {
      fs.mkdirSync(path.join(managers, 'bin'));
      return fs.symlinkSync('/usr/local/bin/' + manager, path.join(managers, 'bin', manager));
    }
  }
  let install = pin[2] ? manager + '@' + pin[2] : manager;
  const npm = path.join(tools, 'node/bin/npm');
  if (pin[3]) {
    const result = execFileSync(npm, ['pack', '--ignore-scripts', '--json', install], { cwd: managers });
    install = path.join(managers, JSON.parse(result)[0].filename);
    if (createHash(pin[3]).update(fs.readFileSync(install)).digest('hex') !== pin[4].toLowerCase()) throw new Error('packageManager checksum mismatch');
  }
  run(npm, ['install', '--global', '--prefix', managers, '--ignore-scripts', install], { cwd: managers });
}

fs.rmSync(managers, { recursive: true, force: true });
fs.rmSync('node_modules', { recursive: true, force: true });
fs.mkdirSync(managers, { recursive: true });
selectNode();
selectManager();
