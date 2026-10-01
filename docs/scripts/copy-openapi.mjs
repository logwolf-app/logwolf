// Copies the API spec at the repository root into public/, so the site serves
// it at /openapi.yaml. The root copy is the one the broker's tests hold against
// its routes; this one is generated, and ignored by git.
import { copyFileSync, mkdirSync } from 'node:fs';

const root = new URL('../../openapi.yaml', import.meta.url);
const target = new URL('../public/openapi.yaml', import.meta.url);

mkdirSync(new URL('.', target), { recursive: true });
copyFileSync(root, target);
