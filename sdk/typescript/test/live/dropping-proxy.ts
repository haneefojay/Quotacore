// A TCP proxy in front of the real service that can let a request through and then lose its
// response: the client's connection is destroyed the moment the service starts answering, so the
// mutation is applied and the client never learns it (IP-26 D14).

import { createServer, connect, type Server, type Socket } from "node:net";
import type { AddressInfo } from "node:net";

export class DroppingProxy {
  /** Connections whose response is lost, counted down as they are used. */
  drop = 0;
  /** Responses lost so far. */
  dropped = 0;
  #server: Server;
  #sockets = new Set<Socket>();

  constructor(private readonly upstream: URL) {
    this.#server = createServer((client) => this.#relay(client));
  }

  async start(): Promise<string> {
    await new Promise<void>((r) => this.#server.listen(0, "127.0.0.1", r));
    return `http://127.0.0.1:${(this.#server.address() as AddressInfo).port}`;
  }

  async stop(): Promise<void> {
    for (const s of this.#sockets) s.destroy();
    await new Promise<void>((r) => this.#server.close(() => r()));
  }

  #relay(client: Socket): void {
    // The decision is made per request, as it arrives, because the client keeps connections alive
    // and a pooled one may carry several requests.
    let lose = false;
    const server = connect(Number(this.upstream.port || 80), this.upstream.hostname);
    for (const s of [client, server]) {
      this.#sockets.add(s);
      s.on("close", () => this.#sockets.delete(s));
      s.on("error", () => {});
    }
    client.on("data", () => {
      if (!lose && this.drop > 0) {
        this.drop--;
        lose = true;
      }
    });
    client.pipe(server);
    server.on("data", (chunk) => {
      if (lose) {
        this.dropped++;
        client.destroy();
        server.destroy();
        return;
      }
      client.write(chunk);
    });
    server.on("end", () => client.end());
    client.on("close", () => server.destroy());
  }
}
