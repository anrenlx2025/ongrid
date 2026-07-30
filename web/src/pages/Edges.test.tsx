import { act } from "react";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it, vi } from "vitest";

import EdgesPage from "./Edges";
import { server } from "@/test/msw-server";

vi.mock("@/store/me", () => ({
  usePermissions: () => ({ isAdmin: true, canMutate: true, role: "admin" }),
}));

describe("EdgesPage", () => {
  beforeEach(() => {
    localStorage.setItem("ongrid-locale", "zh-CN");
    server.use(
      http.get("/api/v1/version", () =>
        HttpResponse.json({ manager_version: "dev" }),
      ),
      http.get("/api/v1/edges", () =>
        HttpResponse.json({
          items: [
            {
              id: 3,
              name: "kind-controller",
              status: "online",
              roles: [],
              access_key_id: "ak-controller",
              last_seen_at: "2026-06-29T10:00:00Z",
              host_info: { hostname: "controller-pod", ip_address: "10.0.0.3" },
              device_id: 3,
              agent_version: "dev",
            },
            {
              id: 5,
              name: "k8s:kind-local:ongrid-k8s-control-plane",
              status: "online",
              roles: [],
              access_key_id: "ak-node",
              last_seen_at: "2026-06-29T10:00:00Z",
              host_info: {
                hostname: "ongrid-k8s-control-plane",
                ip_address: "10.0.0.5",
              },
              device_id: 17,
              agent_version: "dev",
            },
            {
              id: 9,
              name: "bare-metal-1",
              status: "online",
              roles: ["server"],
              access_key_id: "ak-host",
              last_seen_at: "2026-06-29T10:00:00Z",
              host_info: { hostname: "bm-1", ip_address: "10.0.0.9" },
              device_id: 19,
              agent_version: "dev",
            },
          ],
          total: 3,
        }),
      ),
      http.get("/api/v1/devices", () =>
        HttpResponse.json({
          items: [
            {
              id: 3,
              name: "kind-controller",
              hostname: "controller-pod",
              ip_address: "10.0.0.3",
              roles: [],
              online: true,
              last_seen_at: "2026-06-29T10:00:00Z",
            },
            {
              id: 17,
              name: "k8s:kind-local:ongrid-k8s-control-plane",
              hostname: "ongrid-k8s-control-plane",
              ip_address: "10.0.0.5",
              roles: [],
              online: true,
              last_seen_at: "2026-06-29T10:00:00Z",
            },
            {
              id: 19,
              name: "bare-metal-1",
              hostname: "bm-1",
              ip_address: "10.0.0.9",
              roles: ["server"],
              online: true,
              last_seen_at: "2026-06-29T10:00:00Z",
            },
          ],
          total: 3,
        }),
      ),
      http.get("/api/v1/k8s/edge-attachments", () =>
        HttpResponse.json({
          items: [
            {
              edge_id: 3,
              cluster_id: 1,
              cluster_name: "kind-local",
              cluster_mode: "full-node",
              node_name: "ongrid-k8s-control-plane",
              kind: "k8s-controller",
            },
            {
              edge_id: 5,
              cluster_id: 1,
              cluster_name: "kind-local",
              cluster_mode: "full-node",
              node_name: "ongrid-k8s-control-plane",
              kind: "k8s-node",
            },
            {
              edge_id: 5,
              cluster_id: 1,
              cluster_name: "kind-local",
              cluster_mode: "full-node",
              node_name: "ongrid-k8s-control-plane",
              kind: "k8s-controller-runtime",
            },
          ],
          total: 3,
        }),
      ),
    );
  });

  it("隐藏 Controller Edge，并把 K8s Controller 标到所在 Node Edge", async () => {
    render(
      <MemoryRouter>
        <EdgesPage />
      </MemoryRouter>,
    );

    const k8sNameCells = await screen.findAllByText("ongrid-k8s-control-plane");
    expect(k8sNameCells).toHaveLength(2);
    expect(
      screen.queryByText("k8s:kind-local:ongrid-k8s-control-plane"),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("kind-controller")).not.toBeInTheDocument();
    expect(screen.getByText("K8s Node")).toBeInTheDocument();
    expect(screen.getByText("K8s Controller")).toBeInTheDocument();
    expect(screen.getByText("kind-local")).toBeInTheDocument();
    const k8sRow = k8sNameCells[0].closest("tr");
    expect(k8sRow).not.toBeNull();
    expect(
      within(k8sRow as HTMLTableRowElement).queryByText("Kubernetes 管理"),
    ).not.toBeInTheDocument();
    const terminalLink = within(k8sRow as HTMLTableRowElement).getByRole(
      "link",
      { name: /打开.*终端/ },
    );
    expect(terminalLink).toHaveAttribute("href", "/devices/17/shell");
    expect(terminalLink).toHaveAttribute("target", "_blank");
    expect(
      within(k8sRow as HTMLTableRowElement).queryByText("查看图表"),
    ).not.toBeInTheDocument();
    expect(
      within(k8sRow as HTMLTableRowElement).queryByLabelText(/选择/),
    ).not.toBeInTheDocument();
    expect(screen.getByText("bare-metal-1")).toBeInTheDocument();
    expect(screen.getByText("bm-1")).toBeInTheDocument();
    expect(screen.getByText("Host Edge")).toBeInTheDocument();
    expect(screen.getByRole("table")).toHaveClass("w-full", "min-w-[1260px]");
  });

  it("点击 K8s 托管设备行进入设备详情，操作列只保留 WebSSH", async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={["/devices"]}>
        <EdgesPage />
        <LocationProbe />
      </MemoryRouter>,
    );

    const k8sNameCells = await screen.findAllByText("ongrid-k8s-control-plane");
    const k8sRow = k8sNameCells[0].closest("tr") as HTMLTableRowElement;
    expect(k8sRow).not.toBeNull();

    await act(async () => {
      await user.click(k8sRow);
    });
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent("/devices/17"),
    );

    expect(within(k8sRow).queryByText("Kubernetes 管理")).not.toBeInTheDocument();
    expect(
      within(k8sRow).getByRole("link", { name: /打开.*终端/ }),
    ).toHaveAttribute("href", "/devices/17/shell");
    expect(within(k8sRow).queryByText("查看图表")).not.toBeInTheDocument();
    expect(
      within(k8sRow).queryByRole("button", { name: /更多/ }),
    ).not.toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent("/devices/17"),
    );
  });

  it("让设备操作菜单在视口内翻转或滚动", async () => {
    const user = userEvent.setup();
    const originalInnerHeight = window.innerHeight;
    const originalGetBoundingClientRect =
      Element.prototype.getBoundingClientRect;
    let triggerRect = makeRect({
      top: 540,
      left: 1200,
      width: 32,
      height: 32,
    });
    const menuRect = makeRect({ top: 0, left: 0, width: 208, height: 315 });

    Object.defineProperty(window, "innerHeight", {
      configurable: true,
      value: 600,
    });
    const rectSpy = vi
      .spyOn(Element.prototype, "getBoundingClientRect")
      .mockImplementation(function (this: Element) {
        if (this.getAttribute("aria-label") === "更多操作") return triggerRect;
        if (this.getAttribute("role") === "menu") return menuRect;
        return originalGetBoundingClientRect.call(this);
      });

    try {
      render(
        <MemoryRouter>
          <EdgesPage />
        </MemoryRouter>,
      );

      await screen.findByText("bare-metal-1");
      await act(async () => {
        await user.click(screen.getByRole("button", { name: "更多操作" }));
      });

      const menu = await screen.findByRole("menu");
      await waitFor(() => {
        const top = Number.parseFloat(menu.style.top);
        expect(top).toBeLessThan(triggerRect.top);
        expect(top).toBeGreaterThanOrEqual(8);
        expect(top + menuRect.height).toBeLessThanOrEqual(
          window.innerHeight - 8,
        );
      });

      await act(async () => {
        triggerRect = makeRect({
          top: 100,
          left: 1200,
          width: 32,
          height: 32,
        });
        Object.defineProperty(window, "innerHeight", {
          configurable: true,
          value: 240,
        });
        window.dispatchEvent(new Event("resize"));
      });
      await waitFor(() => {
        const top = Number.parseFloat(menu.style.top);
        const maxHeight = Number.parseFloat(menu.style.maxHeight);
        expect(top).toBeGreaterThan(triggerRect.bottom);
        expect(maxHeight).toBeLessThan(menuRect.height);
        expect(top + maxHeight).toBeLessThanOrEqual(window.innerHeight - 8);
      });
    } finally {
      rectSpy.mockRestore();
      Object.defineProperty(window, "innerHeight", {
        configurable: true,
        value: originalInnerHeight,
      });
    }
  });
});

function makeRect({
  top,
  left,
  width,
  height,
}: {
  top: number;
  left: number;
  width: number;
  height: number;
}): DOMRect {
  return {
    x: left,
    y: top,
    top,
    left,
    width,
    height,
    right: left + width,
    bottom: top + height,
    toJSON: () => ({}),
  };
}

function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location">{location.pathname}</div>;
}
