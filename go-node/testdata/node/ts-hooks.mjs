// Lets plain `node` import the TS sources of the TS node (libs/contract,
// src/common/utils/...) directly: TypeScript is transformed by Node itself and
// extensionless relative imports resolve to .ts or /index.ts.
import { existsSync, readFileSync, statSync } from 'node:fs';
import { registerHooks, stripTypeScriptTypes } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import path from 'node:path';

const isFile = (p) => existsSync(p) && statSync(p).isFile();

registerHooks({
    resolve(specifier, context, next) {
        if ((specifier.startsWith('.') || specifier.startsWith('/')) && context.parentURL?.startsWith('file:')) {
            const base = path.resolve(path.dirname(fileURLToPath(context.parentURL)), specifier);
            for (const c of [base, base + '.ts', path.join(base, 'index.ts')]) {
                if (isFile(c)) return { url: pathToFileURL(c).href, shortCircuit: true };
            }
        }
        return next(specifier, context);
    },
    load(url, context, next) {
        if (url.endsWith('.ts')) {
            // 'transform' mode also handles enums (the contract uses them).
            const src = stripTypeScriptTypes(readFileSync(fileURLToPath(url), 'utf8'), { mode: 'transform' });
            return { format: 'module', source: src, shortCircuit: true };
        }
        return next(url, context);
    },
});
