import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ConfigProvider } from "antd";
import { MemoryRouter, useLocation } from "react-router-dom";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import type { InstallPreview } from "@/api/generated/core-client";

import KnowledgePage from "./index";
import * as marketApi from "@/modules/knowledge/api/knowledgeMarket";

vi.mock("react-i18next", async (load) => {
  const { default: messages } = await import("@/i18n/locales/zh-CN");
  const t = (key: string, values: Record<string, unknown> = {}) => {
    const value = key.split(".").reduce<any>((result, part) => result?.[part], messages);
    return typeof value === "string"
      ? value.replace(/{{(\w+)}}/g, (_, name) => String(values[name] ?? ""))
      : key;
  };
  return { ...await load<typeof import("react-i18next")>(), useTranslation: () => ({ t }) };
});
vi.mock("@/modules/knowledge/api/knowledgeMarket", () => ({
  getKnowledgeMarketItem: vi.fn(),
  getKnowledgeMarketTask: vi.fn(),
  deleteKnowledgeMarketTask: vi.fn(),
  retryKnowledgeMarketTask: vi.fn(),
  getKnowledgeMarketInstallPreview: vi.fn(),
  installKnowledgeMarketItem: vi.fn(),
  listKnowledgeMarket: vi.fn(),
  listKnowledgeMarketDomains: vi.fn(),
  listKnowledgeMarketInstalls: vi.fn(),
  listKnowledgeMarketTasks: vi.fn(),
  listKnowledgeMarketTaskPage: vi.fn(),
  listKnowledgeMarketTaskFiles: vi.fn(),
  updateAllKnowledgeMarketItems: vi.fn(),
  updateKnowledgeMarketItem: vi.fn(),
}));
vi.mock("@/modules/knowledge/components/SyncKnowledgeBaseCreationFlow", () => ({
  default: () => null,
  useSyncKnowledgeBaseCreation: () => ({}),
}));
vi.mock("@/components/ui/TypedConfirmModal", async () => ({
  default: (await import("react")).forwardRef(() => null),
}));
vi.mock("@/modules/knowledge/components/UpdateModal", async () => ({
  default: (await import("react")).forwardRef(() => null),
}));
vi.mock("@/modules/knowledge/components/CreateKnowledgeBaseModal", async () => ({
  default: (await import("react")).forwardRef(() => null),
}));
const tableRows = vi.hoisted(() => vi.fn());
vi.mock("@/components/ui", () => ({ ListPageTable: (props: { dataSource?: unknown[] }) => { tableRows(props.dataSource); return null; } }));
vi.mock("@/modules/knowledge/components/KnowledgeTag", () => ({ default: () => null }));
vi.mock("@/components/auth", () => ({ AgentAppsAuth: { getUserInfo: () => ({}) } }));
vi.mock("@/components/request", () => ({
  BASE_URL: "",
  getLocalizedErrorMessage: () => "安装请求失败",
  axiosInstance: { get: vi.fn().mockResolvedValue({ data: { ready: true, items: [], total: 0, page: 1, page_size: 20, counts: {active: 0}, unfinished_count: 0 } }) },
}));
vi.mock("@/modules/knowledge/utils/request", () => ({
  KnowledgeBaseServiceApi: () => ({
    datasetServiceListDatasets: vi.fn().mockResolvedValue({ data: { datasets: [] } }),
  }),
}));
vi.mock("@/modules/dataSource/api/clients", () => ({ dataSourceScanApi: {} }));
vi.mock("@/hooks/useModelFeatures", () => ({
  fetchModelFeatures: vi.fn().mockResolvedValue({}),
  isImageEmbedRequired: () => false,
  MODEL_FEATURES_CHANGED_EVENT: "features-changed",
}));

const jobs = new Map<string, marketApi.KnowledgeMarketTaskDetail>();

function task(id: string, overrides: Partial<marketApi.KnowledgeMarketTaskDetail> = {}) {
  return {
    job_id: id,
    job_type: "knowledge_market_install",
    job_status: "running",
    market_item_id: id,
    name: `知识库 ${id}`,
    created_at: "2026-09-03T08:00:00Z",
    dataset_id: "",
    error_message: "",
    icon: "",
    install_state: "vectorizing",
    attempt_count: 1,
    max_attempts: 3,
    stage: "parsing",
    overall_percent: 65,
    progress: { current: 2, total: 2 },
    parse: { total: 8, done: 1, failed: 0, pending: 0, parsing: 7, state: "parsing" },
    payload: { market_item_id: id },
    ...overrides,
  };
}

function unfinishedCount() {
  return [...jobs.values()].filter((job) => ["waiting", "running"].includes(job.index_progress?.state || "") || (job.display_state
    ? ["pending", "processing", "blocked", "unknown"].includes(job.display_state)
    : ["pending", "running"].includes(job.job_status) || (!["failed", "canceled"].includes(job.job_status) && !["done", "failed", "partial_failed"].includes(job.stage || "")) || (job.stage === "done" && (job.overall_percent || 0) < 100))).length;
}

function installPreview(vectorIndex: InstallPreview["vector_index"] = { available: false }): InstallPreview {
  return { item_id: "install", version: "v1", manifest_sha256: "fixture-sha", document_count: 4, chunk_count: 21, size_bytes: 1536, vector_index: vectorIndex };
}

function marketHTTPError(status: number, data?: { reason: string }) {
  return { isAxiosError: true, response: { status, data: { data } } };
}

function taskEntry(count: number) {
  return screen.getByRole("button", { name: `后台任务，${count} 个任务未完成` });
}

function mockActiveInstall(id: string) {
  vi.mocked(marketApi.listKnowledgeMarketInstalls).mockResolvedValue({
    total: 1,
    items: [{
      market_item_id: id, name: `知识库 ${id}`, active: true,
      dataset_id: "dataset", domain: "测试", icon: "", install_state: "vectorizing",
      installed_version: "1", updated_at: "2026-09-03",
    }],
  });
}

async function mountPage() {
  await act(async () => {
    render(
      <ConfigProvider button={{ autoInsertSpace: false }} theme={{ token: { motion: false } }}>
        <MemoryRouter><KnowledgePage /><CurrentLocation /></MemoryRouter>
      </ConfigProvider>,
    );
  });
}

function CurrentLocation() {
  return <div aria-label="Current location">{useLocation().pathname}</div>;
}

async function click(element: HTMLElement) {
  await act(async () => { fireEvent.click(element); });
  if (element.getAttribute("aria-haspopup") === "dialog" && (element.getAttribute("aria-label") || "").startsWith("后台任务")) {
    await act(async () => { fireEvent.click(screen.getByRole("tab", { name: "安装与更新" })); });
  }
}

beforeAll(() => {
  window.matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches: false, media: query, onchange: null,
    addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {},
    dispatchEvent: () => false,
  }));
  globalThis.ResizeObserver = class {
    observe() {} unobserve() {} disconnect() {}
  };
});

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  jobs.clear();
  vi.mocked(marketApi.listKnowledgeMarket).mockResolvedValue(
    ["install", "update"].map((id) => ({
      id, name: `知识库 ${id}`, category: "industry", domain: "测试",
      description: "测试知识库", tags: [], icon: "", data_source: "官方",
      online_access_url: "", sort_order: 0, version: "2",
      created_at: "2026-09-01", updated_at: "2026-09-03",
    })),
  );
  vi.mocked(marketApi.listKnowledgeMarketDomains).mockResolvedValue({ domains: {} });
  vi.mocked(marketApi.listKnowledgeMarketInstalls).mockResolvedValue({
    total: 1,
    items: [{
      market_item_id: "update", name: "知识库 update", active: false,
      dataset_id: "dataset", domain: "测试", icon: "", install_state: "done",
      installed_version: "1", updated_at: "2026-09-01",
    }],
  });
  vi.mocked(marketApi.listKnowledgeMarketTasks).mockImplementation(async (jobType) => ({
    items: [...jobs.values()].filter((job) => job.job_type === jobType),
    total: jobs.size, page: 1, page_size: 100, unfinished_count: unfinishedCount(),
  }));
  vi.mocked(marketApi.listKnowledgeMarketTaskPage).mockImplementation(async (page) => ({items: [...jobs.values()].sort((a,b) => b.created_at.localeCompare(a.created_at)).slice((page-1)*10,page*10), total: jobs.size, page, page_size: 10, unfinished_count: unfinishedCount()}));
  vi.mocked(marketApi.listKnowledgeMarketTaskFiles).mockImplementation(async (id, status, page) => {
    const parse = jobs.get(id)?.parse;
    const items = status === "failed" ? (parse?.failures || []).map((failure) => ({file_id: failure.task_id || failure.name, name: failure.name, status: "failed" as const, reason_code: failure.reason})) : [];
    return {items, page, page_size: 10, total: items.length, counts: {success: parse?.done || 0, failed: parse?.failed || 0, canceled: parse?.canceled || 0, unfinished: (parse?.pending || 0)+(parse?.parsing || 0)}, file_details_available: true};
  });
  vi.mocked(marketApi.getKnowledgeMarketTask).mockImplementation(async (jobId) => jobs.get(jobId)!);
  vi.mocked(marketApi.getKnowledgeMarketInstallPreview).mockRejectedValue(marketHTTPError(404));
  vi.mocked(marketApi.installKnowledgeMarketItem).mockImplementation(async (id) => {
    jobs.set(id, task(id));
    return { job_id: id, state: "pending" };
  });
  vi.mocked(marketApi.updateKnowledgeMarketItem).mockImplementation(async (id) => {
    jobs.set(id, task(id, { job_type: "knowledge_market_update" }));
    return { job_id: id, state: "pending" };
  });
  vi.mocked(marketApi.updateAllKnowledgeMarketItems).mockImplementation(async () => {
    jobs.set("batch", task("batch", { job_type: "knowledge_market_update_all", market_item_id: "" }));
    return { job_id: "batch", state: "pending" };
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("knowledge market background tasks", () => {
  it.each(["card", "detail"])("confirms a text-only install from the %s and never submits on cancel", async (entry) => {
    vi.mocked(marketApi.getKnowledgeMarketInstallPreview).mockResolvedValue(installPreview());
    await mountPage();
    await click(screen.getByRole("button", { name: /知识广场/ }));
    const card = screen.getByRole("button", { name: "查看知识库 install详情" });
    const open = async () => {
      if (entry === "detail") {
        await click(card);
        await click(within(screen.getByRole("dialog", { name: "知识库 install" })).getByRole("button", { name: "安装" }));
        await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
        expect(screen.queryByRole("dialog", { name: "知识库 install" })).not.toBeInTheDocument();
      } else {
        await click(within(card).getByRole("button", { name: "安装" }));
      }
      return within(screen.getByRole("dialog", { name: "安装 知识库 install" }));
    };
    const dialog = await open();
    expect(dialog.getByRole("radio", { name: "仅安装文本切片" })).toBeChecked();
    expect(dialog.getByRole("radio", { name: "安装并启用向量检索" })).toBeDisabled();
    expect(dialog.getByText("文档数")).toBeInTheDocument();
    expect(dialog.getByText("21")).toBeInTheDocument();
    expect(dialog.getByText("1.5 KB")).toBeInTheDocument();
    await click(dialog.getByRole("button", { name: "取消" }));
    expect(marketApi.installKnowledgeMarketItem).not.toHaveBeenCalled();

    const reopened = await open();
    await click(reopened.getByRole("button", { name: "安装" }));
    expect(vi.mocked(marketApi.installKnowledgeMarketItem).mock.calls).toEqual([["install", { processing_level: "chunked" }, { silentError: true }]]);
    expect(screen.getByText("已加入后台任务")).toBeInTheDocument();
  });

  it("refreshes a stale vector confirmation and waits for explicit confirmation again", async () => {
    vi.mocked(marketApi.getKnowledgeMarketInstallPreview)
      .mockResolvedValueOnce(installPreview({ available: true, model_name: "向量模型 A", confirmation_token: "token-a" }))
      .mockResolvedValueOnce({ ...installPreview({ available: true, model_name: "向量模型 B", confirmation_token: "token-b" }), version: "v2" });
    vi.mocked(marketApi.installKnowledgeMarketItem)
      .mockRejectedValueOnce(marketHTTPError(409, { reason: "install_confirmation_stale" }));
    await mountPage();
    await click(screen.getByRole("button", { name: /知识广场/ }));
    await click(screen.getByRole("button", { name: "安装" }));
    const dialog = within(screen.getByRole("dialog", { name: "安装 知识库 install" }));
    await click(dialog.getByRole("radio", { name: "安装并启用向量检索" }));
    expect(dialog.getByText(/向量模型 A/)).toBeInTheDocument();
    expect(dialog.getByText(/可能产生费用/)).toBeInTheDocument();
    await click(dialog.getByRole("button", { name: "安装" }));
    expect(marketApi.getKnowledgeMarketInstallPreview).toHaveBeenCalledTimes(2);
    expect(vi.mocked(marketApi.installKnowledgeMarketItem).mock.calls).toEqual([["install", { processing_level: "indexed", confirmation_token: "token-a" }, { silentError: true }]]);
    expect(dialog.getByText(/重新确认/)).toBeInTheDocument();
    expect(dialog.getByText("v2")).toBeInTheDocument();
    expect(dialog.getByText(/向量模型 B/)).toBeInTheDocument();

    await click(dialog.getByRole("button", { name: "安装" }));
    expect(marketApi.installKnowledgeMarketItem).toHaveBeenNthCalledWith(2, "install", { processing_level: "indexed", confirmation_token: "token-b" }, { silentError: true });
    expect(screen.getByText("已加入后台任务")).toBeInTheDocument();
  });

  it("shows index failure independently and refreshes a recovered receipt without a retry notification", async () => {
    const indexed = task("indexed", {
      job_status: "succeeded", stage: "done", display_state: "done", overall_percent: 100, dataset_id: "reading-library",
      index_progress: { state: "running", total: 2, done: 1, failed: 0, pending: 1, unknown: 0 },
    });
    jobs.set(indexed.job_id, indexed);
    await mountPage();
    await click(taskEntry(1));
    const dialog = within(screen.getByRole("dialog", { name: "后台任务" }));
    expect(dialog.getByRole("link", { name: indexed.name })).toHaveAttribute("href", "/lib/knowledge/detail/reading-library");
    expect(dialog.getByText("文本安装")).toBeInTheDocument();
    expect(dialog.getByText("100%")).toBeInTheDocument();
    expect(dialog.getByText("正在建立索引")).toBeInTheDocument();
    expect(dialog.getByText("已索引 1/2 篇")).toBeInTheDocument();

    jobs.set(indexed.job_id, { ...indexed, can_retry: true, index_progress: { ...indexed.index_progress!, state: "partial_failed", failed: 1, pending: 0 } });
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(dialog.getByText("部分索引失败")).toBeInTheDocument();
    expect(dialog.getByRole("link", { name: indexed.name })).toBeInTheDocument();
    vi.mocked(marketApi.retryKnowledgeMarketTask).mockResolvedValue({ job_id: indexed.job_id, state: "succeeded" });
    await click(dialog.getByRole("button", { name: "重试" }));
    expect(vi.mocked(marketApi.retryKnowledgeMarketTask).mock.calls).toEqual([[indexed.job_id]]);
    expect(screen.queryByText("已加入后台任务")).not.toBeInTheDocument();
  });

  it.each([true, false])("follows the public installation for an indexing retry (parent returned: %s)", async (parentReturned) => {
    for (let i = 0; i < 10; i += 1) {
      jobs.set(`recent-${i}`, task(`recent-${i}`, { job_status: "succeeded", display_state: "done" }));
    }
    const index = { state: "unknown" as const, total: 2, done: 1, failed: 0, pending: 0, unknown: 1 };
    const parent = task("original-install", {
      job_status: "succeeded", stage: "done", display_state: "done", overall_percent: 100,
      parse: { state: "done", total: 2, done: 2, failed: 0, pending: 0, parsing: 0 },
      index_progress: index, can_retry: true, created_at: "2026-09-01T08:00:00Z",
    });
    jobs.set(parent.job_id, parent);
    vi.mocked(marketApi.retryKnowledgeMarketTask).mockImplementation(async () => {
      jobs.set(parent.job_id, { ...parent, can_retry: false, index_progress: { ...index, state: "running", pending: 1, unknown: 0 } });
      return { job_id: "internal-index-dispatch", state: "pending", ...(parentReturned ? { parent_job_id: parent.job_id } : {}) };
    });

    await mountPage();
    await click(taskEntry(0));
    const dialog = within(screen.getByRole("dialog", { name: "后台任务" }));
    await click(dialog.getByRole("button", { name: "任务下一页" }));
    expect(dialog.getByText("索引状态待确认")).toBeInTheDocument();
    await click(dialog.getByRole("button", { name: "重试" }));

    expect(vi.mocked(marketApi.retryKnowledgeMarketTask).mock.calls).toEqual([[parent.job_id]]);
    expect(dialog.getByRole("button", { name: `收起任务详情 ${parent.name}` })).toHaveAttribute("aria-expanded", "true");
    expect(dialog.getByText("正在建立索引")).toBeInTheDocument();
    expect(screen.getByText(`${parent.name} 的索引重试已提交。`)).toBeInTheDocument();
    expect(marketApi.listKnowledgeMarketTaskFiles).toHaveBeenCalledWith(parent.job_id, "success", 1, expect.anything());
    expect(vi.mocked(marketApi.getKnowledgeMarketTask).mock.calls.every(([id]) => id !== "internal-index-dispatch")).toBe(true);
  });

  it("keeps cached dataset names unlinked until the opened dialog gets its own response", async () => {
    const cached = task("cached", { dataset_id: "ds-old" });
    jobs.set("cached", cached);
    await mountPage();
    let finish!: () => void;
    vi.mocked(marketApi.listKnowledgeMarketTaskPage).mockImplementationOnce(() => new Promise(resolve => {
      finish = () => resolve({ items: [{ ...cached, dataset_deleted: true }], total: 1, page: 1, page_size: 10, unfinished_count: 1 });
    }));
    await click(taskEntry(1));
    const dialog = within(screen.getByRole("dialog"));
    expect(dialog.getByText(cached.name)).toBeInTheDocument();
    expect(dialog.queryByRole("link", { name: cached.name })).toBeNull();
    await act(async () => finish());
    expect(dialog.getByText(cached.name + " · 知识库已删除")).toBeInTheDocument();
    expect(dialog.queryByRole("link", { name: cached.name })).toBeNull();
  });

  it("shows the server's global count independently of locally tracked jobs", async () => {
    vi.mocked(marketApi.listKnowledgeMarketTaskPage).mockResolvedValue({
      items: [], total: 30, page: 1, page_size: 10, unfinished_count: 7,
    });
    await mountPage();
    expect(taskEntry(7).querySelector(".ant-badge")).toHaveTextContent("7");
    expect(screen.queryByText("已加入后台任务")).toBeNull();
    await click(taskEntry(7));
    expect(taskEntry(7)).toHaveAttribute("aria-expanded", "true");
  });

  it("keeps the entry usable without announcing a fake zero when the first query fails", async () => {
    vi.mocked(marketApi.listKnowledgeMarketTaskPage).mockRejectedValue(new Error("offline"));
    await mountPage();
    const entry = screen.getByRole("button", { name: "后台任务" });
    expect(entry.querySelector(".ant-badge-count")).toBeNull();
    expect(screen.queryByRole("button", { name: /0 个任务未完成/ })).toBeNull();
    await click(entry);
    expect(screen.getByRole("dialog", { name: "后台任务" })).toBeInTheDocument();
  });

  it.each([false, true])("disables update all without a count when nothing needs updating (installed: %s)", async (installed) => {
    vi.mocked(marketApi.listKnowledgeMarketInstalls).mockResolvedValue({
      total: installed ? 1 : 0,
      items: installed ? [{
        market_item_id: "update", name: "知识库 update", active: false,
        dataset_id: "dataset", domain: "测试", icon: "", install_state: "done",
        installed_version: "2", updated_at: "2026-09-03",
      }] : [],
    });
    await mountPage();
    await click(screen.getByRole("tab", { name: "已安装的官方资料库" }));
    const button = screen.getByRole("button", { name: "一键更新", exact: true });
    expect(button).toBeDisabled();
    await click(button);
    expect(marketApi.updateAllKnowledgeMarketItems).not.toHaveBeenCalled();
  });

  it("enables update all with the available count and disables it while the batch runs", async () => {
    await mountPage();
    await click(screen.getByRole("tab", { name: "已安装的官方资料库" }));
    const button = screen.getByRole("button", { name: "一键更新 (1)" });
    expect(button).toBeEnabled();
    await click(button);
    expect(button).toBeDisabled();
    await click(button);
    expect(marketApi.updateAllKnowledgeMarketItems).toHaveBeenCalledTimes(1);
  });

  it("announces the batch count and only announces completion when an individual update finishes", async () => {
    await mountPage();
    await click(screen.getByRole("tab", { name: "已安装的官方资料库" }));
    await click(screen.getByRole("button", { name: "一键更新 (1)" }));
    expect(screen.getByText("已加入后台任务")).toBeInTheDocument();
    expect(screen.getByText("1 个知识库将在后台继续更新，你可以继续操作当前页面。")).toBeInTheDocument();
    jobs.set("batch", task("batch", { job_type: "knowledge_market_update_all", job_status: "succeeded" }));
    jobs.set("update", task("update", { job_type: "knowledge_market_update" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(screen.queryByText("更新完成")).toBeNull();
    expect(screen.getByText("批量更新检查已完成")).toBeInTheDocument();
    jobs.set("update", task("update", {
      job_type: "knowledge_market_update", job_status: "succeeded", stage: "done", overall_percent: 100,
      dataset_id: "updated-dataset",
    }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(screen.getByText("更新完成")).toBeInTheDocument();
    expect(screen.getByText("知识库 update 已可在对话中使用。")).toBeInTheDocument();
    await click(screen.getByRole("button", { name: "查看详情" }));
    expect(screen.getByLabelText("Current location")).toHaveTextContent("/lib/knowledge/detail/updated-dataset");
  });

  it("opens background task details if a completed update has no dataset ID", async () => {
    await mountPage();
    await click(screen.getByRole("button", { name: /知识广场/ }));
    await click(screen.getByRole("button", { name: "检查更新" }));
    expect(screen.getByText("已加入后台任务")).toBeInTheDocument();
    jobs.set("update", task("update", {
      job_type: "knowledge_market_update", job_status: "succeeded", stage: "done", overall_percent: 100,
    }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    await click(screen.getByRole("button", { name: "查看详情" }));
    expect(screen.getByRole("dialog", { name: "后台任务" })).toBeInTheDocument();
    expect(screen.getByLabelText("Current location")).toHaveTextContent(/^\/$/);
  });

  it.each([false, true])("dismisses the submitted notice after five seconds while the task continues (hover: %s)", async (hover) => {
    await mountPage();
    await click(screen.getByRole("button", { name: /知识广场/ }));
    await click(screen.getByRole("button", { name: "安装" }));
    if (hover) {
      fireEvent.mouseEnter(screen.getByRole("status"));
    }

    await act(async () => { await vi.advanceTimersByTimeAsync(4999); });
    expect(screen.getByText("已加入后台任务")).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(screen.queryByText("已加入后台任务")).toBeNull();
    expect(taskEntry(1)).toBeInTheDocument();
    await click(taskEntry(1));
    expect(within(screen.getByRole("dialog")).getByText("知识库 install")).toBeInTheDocument();
  });

  it("expires a completion notice created while an earlier notice is hovered", async () => {
    await mountPage();
    await click(screen.getByRole("button", { name: /知识广场/ }));
    await click(screen.getByRole("button", { name: "安装" }));
    fireEvent.mouseEnter(screen.getByRole("status"));
    jobs.set("install", task("install", { job_status: "succeeded", stage: "done", overall_percent: 100 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(screen.getByText("已完成任务")).toBeInTheDocument();

    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    expect(screen.queryByText("已加入后台任务")).toBeNull();
    await act(async () => { await vi.advanceTimersByTimeAsync(1999); });
    expect(screen.getByText("已完成任务")).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(screen.queryByText("已完成任务")).toBeNull();
    await click(taskEntry(0));
    expect(within(screen.getByRole("dialog")).getByText("知识库 install")).toBeInTheDocument();
  });

  it("keeps a failed install with a dataset visible for inspection and uninstall", async () => {
    vi.mocked(marketApi.listKnowledgeMarketInstalls).mockResolvedValue({ total: 1, items: [{
      market_item_id: "update", name: "知识库 update", active: false, dataset_id: "dataset",
      domain: "测试", icon: "", install_state: "failed", installed_version: "1", updated_at: "2026-09-01",
    }] });
    await mountPage();
    await click(screen.getByRole("tab", { name: "已安装的官方资料库" }));
    expect(tableRows).toHaveBeenLastCalledWith(expect.arrayContaining([expect.objectContaining({ id: "update", datasetId: "dataset", installState: "failed" })]));
  });
  it("keeps completed history and deletes only the selected record", async () => {
    jobs.set("complete", task("complete", { job_status: "succeeded", stage: "done", overall_percent: 100, can_delete: true }));
    vi.mocked(marketApi.deleteKnowledgeMarketTask).mockImplementation(async (id) => { jobs.delete(id); });
    await mountPage();
    await click(taskEntry(0));
    const dialog = screen.getByRole("dialog", { name: "后台任务" });
    expect(within(dialog).getByText("知识库 complete")).toBeInTheDocument();
    await click(within(dialog).getByRole("button", { name: "删除" }));
    expect(screen.getByText("仅删除历史记录，保留知识库和文档。")).toBeInTheDocument();
    await click(screen.getByRole("button", { name: /OK|确定/ }));
    expect(marketApi.deleteKnowledgeMarketTask).toHaveBeenCalledWith("complete");
    expect(within(dialog).queryByText("知识库 complete")).toBeNull();
    expect(screen.getByText("删除任务成功")).toBeInTheDocument();
  });

  it("shows partial success counts and failed files and can retry", async () => {
    jobs.set("partial", task("partial", {
      job_status: "succeeded", stage: "partial_failed", overall_percent: 100, can_retry: true, can_delete: true,
      parse: { state: "partial_failed", total: 100, done: 99, failed: 1, pending: 0, parsing: 0,
        failures: [{ task_id: "bad-file", name: "broken.pdf", reason: "parse_failed" }] },
    }));
    vi.mocked(marketApi.retryKnowledgeMarketTask).mockImplementation(async () => {
      jobs.set("retry", task("retry", { market_item_id: "partial", job_status: "pending", stage: "pending", overall_percent: 0, created_at: "2026-09-04T08:00:00Z" }));
      return { job_id: "retry", state: "pending" };
    });
    await mountPage();
    await click(taskEntry(0));
    const dialog = screen.getByRole("dialog", { name: "后台任务" });
    expect(within(dialog).getByText("已完成（部分失败）")).toBeInTheDocument();
    await click(within(dialog).getByRole("button", { name: /展开任务详情/ }));
    expect(within(dialog).getByRole("tab", {name: "成功 99"})).toBeInTheDocument();
    expect(within(dialog).getByRole("tab", {name: "失败 1"})).toBeInTheDocument();
    expect(within(dialog).getByText(/broken.pdf/)).toBeInTheDocument();
    await click(within(dialog).getAllByRole("button", { name: "重试" })[0]);
    expect(marketApi.retryKnowledgeMarketTask).toHaveBeenCalledWith("partial");
    expect(taskEntry(1)).toBeInTheDocument();
    expect(screen.getByText("已加入后台任务").closest(".ant-notification-bottomRight")).not.toBeNull();
    expect(screen.getByText("知识库 partial 已创建新的重试任务，将在后台继续处理。")).toBeInTheDocument();
  });
  it("shows a concise empty background history message", async () => {
    await mountPage();
    await click(taskEntry(0));
    expect(within(screen.getByRole("dialog", { name: "后台任务" })).getByText("暂无后台任务")).toBeInTheDocument();
    expect(screen.queryByText(/暂无 Knowledge Market/)).not.toBeInTheDocument();
  });

  it('shows how many installed knowledge bases have updates', async () => {
    await mountPage();
    await click(screen.getByRole('tab', { name: '已安装的官方资料库' }));
    expect(screen.getByRole('button', { name: '一键更新 (1)' })).toBeInTheDocument();
  });
  it.each([
    ["install", "安装", "安装中"],
    ["update", "检查更新", "更新中"],
  ])("labels a submitted %s task in its card and detail", async (id, action, label) => {
    await mountPage();
    await click(screen.getByRole("button", { name: /知识广场/ }));
    const card = screen.getByRole("button", { name: `查看知识库 ${id}详情` });
    mockActiveInstall(id);
    await click(within(card).getByRole("button", { name: action }));
    expect(within(card).getByRole("button", { name: new RegExp(`${label}$`) })).toBeDisabled();
    vi.mocked(marketApi.getKnowledgeMarketItem).mockResolvedValue({
      category: "industry", data_source: "官方", description: "测试知识库",
      domain: "测试", icon: "", id, name: `知识库 ${id}`, sample_questions: [],
      tags: [], updated_at: "2026-09-03", version: "2", version_date: "", version_note: "",
      created_at: "2026-09-03", package_revision: "2", package_url: "", sort_order: 0,
    });
    await click(card);
    expect(within(screen.getByRole("dialog")).getByRole("button", {
      name: new RegExp(`${label}$`),
    })).toBeDisabled();
  });

  it("restores the updating label while parsing continues after job success", async () => {
    mockActiveInstall("update");
    jobs.set("update", task("update", {
      job_type: "knowledge_market_update", job_status: "succeeded",
    }));
    await mountPage();
    await click(screen.getByRole("button", { name: /知识广场/ }));
    const card = screen.getByRole("button", { name: "查看知识库 update详情" });
    expect(within(card).getByRole("button", { name: /更新中$/ })).toBeDisabled();
  });

  it("uses a neutral processing label when the active task type is unavailable", async () => {
    mockActiveInstall("update");
    await mountPage();
    await click(screen.getByRole("button", { name: /知识广场/ }));
    const card = screen.getByRole("button", { name: "查看知识库 update详情" });
    expect(within(card).getByRole("button", { name: /处理中$/ })).toBeDisabled();
  });

  it("tracks install and update counts, shows task details, and announces completion at bottom right", async () => {
    await mountPage();
    expect(taskEntry(0).querySelector(".ant-badge-count")).toBeNull();
    await click(screen.getByRole("button", { name: /知识广场/ }));
    await click(screen.getByRole("button", { name: "安装" }));
    expect(vi.mocked(marketApi.installKnowledgeMarketItem).mock.calls).toEqual([["install"]]);
    expect(taskEntry(1).querySelector(".ant-badge")).toHaveTextContent("1");
    expect(screen.getByText("已加入后台任务").closest(".ant-notification-bottomRight")).not.toBeNull();
    expect(document.querySelector(".ant-message-notice")).toBeNull();

    await click(screen.getByRole("button", { name: "检查更新" }));
    expect(taskEntry(2).querySelector(".ant-badge")).toHaveTextContent("2");
    await click(taskEntry(2));
    const dialog = screen.getByRole("dialog");
    for (const label of ["任务名称", "任务类型", "状态", "进度", "创建时间"]) {
      expect(within(dialog).getAllByText(label).length).toBeGreaterThan(0);
    }
    expect(within(dialog).getByText("知识库 install")).toBeInTheDocument();
    expect(within(dialog).getByText("知识库 update")).toBeInTheDocument();
    expect(within(dialog).getAllByText("65%")).toHaveLength(2);
    expect(within(dialog).getAllByText(new Date("2026-09-03T08:00:00Z").toLocaleString())).toHaveLength(2);
    await click(within(dialog).getByRole("button", { name: "Close" }));

    jobs.set("install", task("install", { job_status: "succeeded", stage: "done", overall_percent: 86 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(taskEntry(2)).toBeInTheDocument();
    expect(screen.queryByText("已完成任务")).toBeNull();

    jobs.set("install", task("install", { job_status: "succeeded", stage: "done", overall_percent: 100 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(taskEntry(1)).toBeInTheDocument();
    expect(screen.getByText("已完成任务").closest(".ant-notification-bottomRight")).not.toBeNull();

    jobs.set("update", task("update", { job_type: "knowledge_market_update", job_status: "succeeded", stage: "done", overall_percent: 100 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(taskEntry(0)).toBeInTheDocument();
    expect(screen.getAllByText("已完成任务")).toHaveLength(1);
    expect(screen.getByText("更新完成")).toBeInTheDocument();
    expect(document.querySelector(".ant-message-notice")).toBeNull();
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(taskEntry(0).querySelector(".ant-badge-count")).toBeNull();
  });

  it("restores active tasks without announcing historical completions, and recovers from a polling failure", async () => {
    jobs.set("running", task("running"));
    jobs.set("finished", task("finished", { job_status: "succeeded", stage: "done", overall_percent: 100 }));
    await mountPage();
    expect(taskEntry(1)).toBeInTheDocument();
    expect(screen.queryByText("已完成任务")).toBeNull();
    expect(screen.queryByText("已加入后台任务")).toBeNull();
    vi.mocked(marketApi.getKnowledgeMarketTask).mockRejectedValueOnce(new Error("network"));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(taskEntry(1)).toBeInTheDocument();
    jobs.set("running", task("running", { job_status: "succeeded", stage: "done", overall_percent: 100 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(taskEntry(0)).toBeInTheDocument();
    expect(screen.getAllByText("已完成任务")).toHaveLength(1);
  });

  it("counts a batch update and removes failed tasks without a success notification", async () => {
    await mountPage();
    await click(screen.getByRole("tab", { name: "已安装的官方资料库" }));
    await click(screen.getByRole("button", { name: /一键更新/ }));
    expect(taskEntry(1)).toBeInTheDocument();
    expect(screen.getByText("已加入后台任务")).toBeInTheDocument();
    jobs.set("batch", task("batch", { job_type: "knowledge_market_update_all", job_status: "failed", stage: "failed" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(taskEntry(0)).toBeInTheDocument();
    expect(screen.queryByText("已完成任务")).toBeNull();
    expect(screen.queryByText("更新完成")).toBeNull();
    expect(screen.getByText(/批量检查 处理失败/).closest(".ant-notification-bottomRight")).not.toBeNull();
  });

  it("does not count historical installs again while the same knowledge base is updating", async () => {
    jobs.set("old-install", task("old-install", {
      market_item_id: "same-item", job_status: "succeeded", display_state: "done", created_at: "2026-09-01T08:00:00Z",
    }));
    jobs.set("new-update", task("new-update", {
      market_item_id: "same-item", job_type: "knowledge_market_update",
    }));
    await mountPage();
    expect(taskEntry(1)).toBeInTheDocument();
    await click(taskEntry(1));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText("知识库 new-update")).toBeInTheDocument();
    expect(within(dialog).getByText("知识库 old-install")).toBeInTheDocument();
  });

  it("keeps counting updates spawned by a completed batch check", async () => {
    await mountPage();
    await click(screen.getByRole("tab", { name: "已安装的官方资料库" }));
    await click(screen.getByRole("button", { name: /一键更新/ }));
    jobs.set("batch", task("batch", {
      market_item_id: "", job_type: "knowledge_market_update_all", job_status: "succeeded", display_state: "done",
    }));
    jobs.set("child", task("child", { job_type: "knowledge_market_update" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(taskEntry(1)).toBeInTheDocument();
    jobs.set("child", task("child", {
      job_type: "knowledge_market_update", job_status: "succeeded", stage: "done", overall_percent: 100,
    }));
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(taskEntry(0)).toBeInTheDocument();
    expect(screen.getByText("批量更新检查已完成")).toBeInTheDocument();
    expect(screen.getByText("更新完成")).toBeInTheDocument();
  });

  it("does not add a task or show a submitted notification when submission fails", async () => {
    vi.mocked(marketApi.installKnowledgeMarketItem).mockRejectedValueOnce(new Error("failed"));
    await mountPage();
    await click(screen.getByRole("button", { name: /知识广场/ }));
    await click(screen.getByRole("button", { name: "安装" }));
    expect(taskEntry(0)).toBeInTheDocument();
    expect(screen.queryByText("已加入后台任务")).toBeNull();
  });
});
