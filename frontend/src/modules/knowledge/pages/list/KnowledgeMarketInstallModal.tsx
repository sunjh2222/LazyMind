import { useCallback, useEffect, useRef, useState } from "react";
import { Alert, Button, Descriptions, Modal, Radio, Space, Spin, Typography, type RadioChangeEvent } from "antd";
import { isAxiosError } from "axios";
import { useTranslation } from "react-i18next";
import type { InstallPreview, InstallRequest, InstallRequestProcessingLevelEnum } from "@/api/generated/core-client";
import { getLocalizedErrorMessage } from "@/components/request";
import { formatBytes } from "@/modules/dataSource/utils/format";
import { getKnowledgeMarketInstallPreview } from "@/modules/knowledge/api/knowledgeMarket";
import type { OfficialKnowledgeBase } from "./knowledgeSquareData";

interface Props {
  item: OfficialKnowledgeBase;
  onClose: () => void;
  onInstall: (item: OfficialKnowledgeBase, request?: InstallRequest) => Promise<void>;
}

export default function KnowledgeMarketInstallModal({ item, onClose, onInstall }: Props) {
  const { t } = useTranslation();
  const [preview, setPreview] = useState<InstallPreview>();
  const [level, setLevel] = useState<InstallRequestProcessingLevelEnum>("chunked");
  const [loading, setLoading] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [needsConfirmation, setNeedsConfirmation] = useState(false);
  const [error, setError] = useState<{ phase: "preview" | "submit"; message?: string }>();
  const request = useRef<AbortController>();
  const submitInFlight = useRef(false);

  const loadPreview = useCallback(async (signal: AbortSignal, allowLegacy: boolean) => {
    setLoading(true);
    setPreview(undefined);
    setError(undefined);
    try {
      const next = await getKnowledgeMarketInstallPreview(item.id, { signal, silentError: true });
      if (signal.aborted) return;
      setPreview(next);
      if (!next.vector_index.available || !next.vector_index.model_name || !next.vector_index.confirmation_token) setLevel("chunked");
    } catch (failure) {
      if (signal.aborted) return;
      if (allowLegacy && isAxiosError(failure) && failure.response?.status === 404) {
        if (submitInFlight.current) return;
        submitInFlight.current = true;
        setSubmitting(true);
        try {
          await onInstall(item);
        } catch {
          // The legacy request uses the common error notification.
        } finally {
          if (!signal.aborted) onClose();
        }
        return;
      }
      setError({ phase: "preview" });
    } finally {
      if (!signal.aborted) setLoading(false);
    }
  }, [item, onClose, onInstall]);

  useEffect(() => {
    const controller = new AbortController();
    request.current = controller;
    void loadPreview(controller.signal, true);
    return () => controller.abort();
  }, [loadPreview]);

  const vector = preview?.vector_index;
  const confirmationToken = vector?.available && vector.model_name ? vector.confirmation_token : undefined;

  async function submit() {
    const signal = request.current?.signal;
    if (!signal || signal.aborted || !preview || submitInFlight.current || (level === "indexed" && !confirmationToken)) return;
    submitInFlight.current = true;
    setSubmitting(true);
    setError(undefined);
    try {
      await onInstall(item, level === "indexed"
        ? { processing_level: level, confirmation_token: confirmationToken }
        : { processing_level: level });
      if (!signal.aborted) onClose();
    } catch (failure) {
      if (signal.aborted) return;
      if (level === "indexed" && isAxiosError<{ data?: { reason?: string } }>(failure) &&
          failure.response?.status === 409 && failure.response.data?.data?.reason === "install_confirmation_stale") {
        setNeedsConfirmation(true);
        await loadPreview(signal, false);
      } else {
        setError({ phase: "submit", message: getLocalizedErrorMessage(failure) });
      }
    } finally {
      submitInFlight.current = false;
      if (!signal.aborted) setSubmitting(false);
    }
  }

  return <Modal
    title={t("knowledge.marketInstallTitle", { name: item.name })}
    open
    width={560}
    centered
    okText={t("common.install")}
    cancelText={t("common.cancel")}
    confirmLoading={submitting}
    okButtonProps={{ disabled: loading || !preview || (level === "indexed" && !confirmationToken) }}
    cancelButtonProps={{ disabled: submitting }}
    closable={!submitting}
    keyboard={!submitting}
    maskClosable={!submitting}
    onOk={() => void submit()}
    onCancel={() => {
      if (submitInFlight.current) return;
      request.current?.abort();
      onClose();
    }}
  >
    <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      {needsConfirmation && <Alert type="warning" showIcon message={t("knowledge.marketInstallReconfirm")} />}
      {loading && <Space><Spin size="small" />{t("knowledge.marketInstallLoading")}</Space>}
      {error?.phase === "preview" && <Alert type="error" showIcon message={t("knowledge.marketInstallPreviewFailed")}
        action={<Button size="small" onClick={() => {
          const signal = request.current?.signal;
          if (signal) void loadPreview(signal, !needsConfirmation);
        }}>{t("common.retry")}</Button>} />}
      {error?.phase === "submit" && <Alert type="error" showIcon message={error.message || t("knowledge.marketInstallSubmitFailed")} />}
      {preview && <>
        <Descriptions size="small" column={2} items={[
          { key: "version", label: t("knowledge.marketInstallVersion"), children: preview.version },
          { key: "size", label: t("knowledge.marketInstallSize"), children: formatBytes(preview.size_bytes) },
          { key: "documents", label: t("knowledge.marketInstallDocuments"), children: preview.document_count },
          { key: "chunks", label: t("knowledge.marketInstallChunks"), children: preview.chunk_count },
        ]} />
        <Radio.Group value={level} disabled={submitting} aria-label={t("knowledge.processingLevel")}
          onChange={(event: RadioChangeEvent) => setLevel(event.target.value === "indexed" ? "indexed" : "chunked")}>
          <Space direction="vertical">
            <Radio value="chunked">{t("knowledge.marketInstallChunked")}</Radio>
            <Radio value="indexed" disabled={!confirmationToken}>{t("knowledge.marketInstallIndexed")}</Radio>
          </Space>
        </Radio.Group>
        {level === "chunked" && <Typography.Text type="secondary">{t("knowledge.marketInstallChunkedHint")}</Typography.Text>}
        {!confirmationToken && <Typography.Text type="secondary">{t("knowledge.marketInstallModelUnavailable")}</Typography.Text>}
        {level === "indexed" && confirmationToken && <Alert type="info" showIcon
          message={t("knowledge.marketInstallModel", { model: preview.vector_index.model_name })}
          description={t("knowledge.marketInstallIndexEstimate")} />}
      </>}
    </Space>
  </Modal>;
}
