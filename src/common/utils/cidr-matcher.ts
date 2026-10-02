import net from 'node:net';

export class CidrMatcher {
    private readonly ipv4Ranges: { network: number; mask: number }[] = [];

    constructor(cidrs: string[]) {
        for (const cidr of cidrs) {
            const trimmed = cidr.trim();
            if (!trimmed) continue;

            const [ip, prefixStr] = trimmed.split('/');
            const family = net.isIP(ip);

            if (family === 4) {
                const prefix = prefixStr !== undefined ? parseInt(prefixStr, 10) : 32;
                if (prefix >= 0 && prefix <= 32) {
                    const mask = prefix === 0 ? 0 : (~0 << (32 - prefix)) >>> 0;
                    const numIp = this.ipv4ToNumber(ip);
                    this.ipv4Ranges.push({ network: (numIp & mask) >>> 0, mask });
                }
            }
        }
    }

    public contains(ip: string): boolean {
        const family = net.isIP(ip);
        if (family === 4) {
            const numIp = this.ipv4ToNumber(ip);
            for (const range of this.ipv4Ranges) {
                if ((numIp & range.mask) >>> 0 === range.network) {
                    return true;
                }
            }
        }
        return false;
    }

    private ipv4ToNumber(ip: string): number {
        return (
            ip.split('.').reduce((acc, octet) => ((acc << 8) + parseInt(octet, 10)) >>> 0, 0) >>> 0
        );
    }
}
