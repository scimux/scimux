/* Delete only the stage value originally saved with this file. */
export async function removePeopleStage(api, item) {
  if (!item || typeof item.peopleKey !== "string" || !/^[0-9a-f]{32}\/[0-9a-f]{32}\/[0-9a-f]{16}\/[0-9a-f]{16}$/.test(item.peopleKey) ||
      typeof item.ref?.id !== "string" || !/^[0-9a-f]{16}$/.test(item.ref.id) ||
      typeof item.copyInstance !== "string" || !/^[0-9a-f]{32}$/.test(item.copyInstance) ||
      typeof item.stageRevision !== "string" || !/^[0-9a-f]{64}$/.test(item.stageRevision)) throw new Error("Review the current local file before removing this saved file.");
  if (typeof api !== "function") throw new Error("Stage removal is unavailable");
  const path = `/api/team/subscriptions/${item.peopleKey.split("/").map(encodeURIComponent).join("/")}/attachments/stage/${encodeURIComponent(item.ref.id)}?copy_instance=${encodeURIComponent(item.copyInstance)}&revision=${encodeURIComponent(item.stageRevision)}${item.instance ? `&chat_instance=${encodeURIComponent(item.instance)}` : ""}`;
  await api(path, {method:"DELETE"});
}
