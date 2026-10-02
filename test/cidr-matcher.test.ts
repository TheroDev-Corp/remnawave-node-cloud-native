import { describe, expect, it } from 'bun:test';

import { CidrMatcher } from '../src/common/utils/cidr-matcher';

describe('CidrMatcher', () => {
    const matcher = new CidrMatcher([
        '10.0.0.0/8',
        '172.16.0.0/12',
        '192.168.1.0/24',
        '1.2.3.4/32',
    ]);

    it('should correctly match IPs within private CIDR ranges', () => {
        expect(matcher.contains('10.244.0.1')).toBe(true);
        expect(matcher.contains('10.0.0.5')).toBe(true);
        expect(matcher.contains('172.16.0.1')).toBe(true);
        expect(matcher.contains('172.31.255.255')).toBe(true);
        expect(matcher.contains('192.168.1.50')).toBe(true);
        expect(matcher.contains('1.2.3.4')).toBe(true);
    });

    it('should reject IPs outside configured CIDR ranges', () => {
        expect(matcher.contains('1.1.1.1')).toBe(false);
        expect(matcher.contains('8.8.8.8')).toBe(false);
        expect(matcher.contains('172.32.0.1')).toBe(false);
        expect(matcher.contains('192.168.2.1')).toBe(false);
        expect(matcher.contains('93.184.216.34')).toBe(false);
    });

    it('should gracefully handle empty and invalid input', () => {
        const emptyMatcher = new CidrMatcher([]);
        expect(emptyMatcher.contains('10.0.0.1')).toBe(false);
        expect(matcher.contains('invalid-ip')).toBe(false);
        expect(matcher.contains('')).toBe(false);
    });
});
