import { Typography } from "@mui/material";

import HttpStatusIcon from "./HttpStatusIcon";

import { HttpProtocol } from "lib/graphql/generated";

type ResponseStatusProps = {
  proto: HttpProtocol;
  statusCode: number;
  statusReason: string;
};

function mapProto(proto: HttpProtocol): string {
  switch (proto) {
    case HttpProtocol.Http10:
      return "HTTP/1.0";
    case HttpProtocol.Http11:
      return "HTTP/1.1";
    case HttpProtocol.Http20:
      return "HTTP/2.0";
    default:
      return proto;
  }
}

export default function ResponseStatus({ proto, statusCode, statusReason }: ResponseStatusProps): JSX.Element {
  return (
    <Typography variant="h6" style={{ fontSize: "1rem", whiteSpace: "nowrap" }}>
      <HttpStatusIcon status={statusCode} />{" "}
      <Typography component="span" color="textSecondary">
        <Typography component="span" color="textSecondary" style={{ fontFamily: "'JetBrains Mono', monospace" }}>
          {mapProto(proto)}
        </Typography>
      </Typography>{" "}
      {statusCode} {statusReason}
    </Typography>
  );
}
