import { useEffect, useMemo, useState } from "react";
import { currentHash, parseRoute, subscribeRoute, type Route } from "../lib/router";

export function useRoute(): Route {
  const [hash, setHash] = useState(() => currentHash());

  useEffect(() => subscribeRoute(() => setHash(currentHash())), []);

  return useMemo(() => parseRoute(hash), [hash]);
}
