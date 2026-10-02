import fs from 'node:fs';
import https from 'node:https';

export interface IK8sLease {
    apiVersion: string;
    kind: string;
    metadata: {
        name: string;
        namespace: string;
        resourceVersion?: string;
    };
    spec: {
        holderIdentity?: string | null;
        leaseDurationSeconds?: number;
        acquireTime?: string;
        renewTime?: string;
        leaseTransitions?: number;
    };
}

export class K8sLeaseClient {
    private readonly baseUrl: string;
    private readonly token: string;
    private readonly agent: https.Agent;

    constructor() {
        const host = process.env.KUBERNETES_SERVICE_HOST ?? 'kubernetes.default.svc';
        const port = process.env.KUBERNETES_SERVICE_PORT ?? '443';
        this.baseUrl = `https://${host}:${port}`;

        const tokenPath = '/var/run/secrets/kubernetes.io/serviceaccount/token';
        const caPath = '/var/run/secrets/kubernetes.io/serviceaccount/ca.crt';

        this.token = fs.existsSync(tokenPath) ? fs.readFileSync(tokenPath, 'utf8').trim() : '';

        const ca = fs.existsSync(caPath) ? fs.readFileSync(caPath) : undefined;
        this.agent = new https.Agent({ ca, rejectUnauthorized: Boolean(ca) });
    }

    public async getLease(
        namespace: string,
        name: string,
    ): Promise<{ status: number; lease: IK8sLease | null }> {
        const path = `/apis/coordination.k8s.io/v1/namespaces/${encodeURIComponent(namespace)}/leases/${encodeURIComponent(name)}`;
        const res = await this.request('GET', path);
        return {
            status: res.statusCode,
            lease: res.statusCode === 200 ? (JSON.parse(res.body) as IK8sLease) : null,
        };
    }

    public async createLease(
        namespace: string,
        lease: IK8sLease,
    ): Promise<{ status: number; lease: IK8sLease | null }> {
        const path = `/apis/coordination.k8s.io/v1/namespaces/${encodeURIComponent(namespace)}/leases`;
        const res = await this.request('POST', path, JSON.stringify(lease));
        return {
            status: res.statusCode,
            lease: res.statusCode === 201 ? (JSON.parse(res.body) as IK8sLease) : null,
        };
    }

    public async updateLease(
        namespace: string,
        name: string,
        lease: IK8sLease,
    ): Promise<{ status: number; lease: IK8sLease | null }> {
        const path = `/apis/coordination.k8s.io/v1/namespaces/${encodeURIComponent(namespace)}/leases/${encodeURIComponent(name)}`;
        const res = await this.request('PUT', path, JSON.stringify(lease));
        return {
            status: res.statusCode,
            lease: res.statusCode === 200 ? (JSON.parse(res.body) as IK8sLease) : null,
        };
    }

    public async patchPodLabels(
        namespace: string,
        name: string,
        labels: Record<string, string | null>,
    ): Promise<{ status: number; body?: string }> {
        const path = `/api/v1/namespaces/${encodeURIComponent(namespace)}/pods/${encodeURIComponent(name)}`;
        const patchBody = JSON.stringify({
            metadata: {
                labels,
            },
        });
        const res = await this.request(
            'PATCH',
            path,
            patchBody,
            'application/strategic-merge-patch+json',
        );
        return {
            status: res.statusCode,
            body: res.body,
        };
    }

    private request(
        method: string,
        path: string,
        body?: string,
        contentType: string = 'application/json',
    ): Promise<{ statusCode: number; body: string }> {
        return new Promise((resolve, reject) => {
            const url = new URL(path, this.baseUrl);
            const headers: Record<string, string> = {
                Accept: 'application/json',
                Authorization: `Bearer ${this.token}`,
            };

            if (body) {
                headers['Content-Type'] = contentType;
                headers['Content-Length'] = Buffer.byteLength(body).toString();
            }

            const req = https.request(
                url,
                {
                    method,
                    headers,
                    agent: this.agent,
                    timeout: 5000,
                },
                (res) => {
                    let data = '';
                    res.on('data', (chunk) => {
                        data += chunk;
                    });
                    res.on('end', () => {
                        resolve({
                            statusCode: res.statusCode ?? 500,
                            body: data,
                        });
                    });
                },
            );

            req.on('error', (err) => reject(err));
            req.on('timeout', () => {
                req.destroy(new Error('K8s API request timeout'));
            });

            if (body) {
                req.write(body);
            }
            req.end();
        });
    }
}
