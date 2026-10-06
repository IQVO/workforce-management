import type { SidebarsConfig } from "@docusaurus/plugin-content-docs";

const sidebar: SidebarsConfig = {
  apisidebar: [
    {
      type: "doc",
      id: "api-reference/rest/workforce-management-api",
    },
    {
      type: "category",
      label: "Associates",
      link: {
        type: "doc",
        id: "api-reference/rest/associates",
      },
      items: [
        {
          type: "doc",
          id: "api-reference/rest/start-associate-shift",
          label: "Start an associate's shift",
          className: "api-method post",
        },
        {
          type: "doc",
          id: "api-reference/rest/certify-associate",
          label: "Add a certification to an associate",
          className: "api-method post",
        },
        {
          type: "doc",
          id: "api-reference/rest/start-associate-break",
          label: "Start a logged break for an associate",
          className: "api-method post",
        },
        {
          type: "doc",
          id: "api-reference/rest/end-associate-break",
          label: "End a logged break for an associate",
          className: "api-method post",
        },
        {
          type: "doc",
          id: "api-reference/rest/end-associate-shift",
          label: "End an associate's shift",
          className: "api-method post",
        },
      ],
    },
    {
      type: "category",
      label: "Shift Plans",
      link: {
        type: "doc",
        id: "api-reference/rest/shift-plans",
      },
      items: [
        {
          type: "doc",
          id: "api-reference/rest/propose-path-plan",
          label: "Propose planned heads for a path (pure computation, not committed)",
          className: "api-method post",
        },
        {
          type: "doc",
          id: "api-reference/rest/commit-shift-plan",
          label: "Commit a shift's headcount split across paths",
          className: "api-method post",
        },
      ],
    },
    {
      type: "category",
      label: "Assignments",
      link: {
        type: "doc",
        id: "api-reference/rest/assignments",
      },
      items: [
        {
          type: "doc",
          id: "api-reference/rest/assign-labor",
          label: "Assign an associate to a path",
          className: "api-method post",
        },
      ],
    },
    {
      type: "category",
      label: "Staffing",
      link: {
        type: "doc",
        id: "api-reference/rest/staffing",
      },
      items: [
        {
          type: "doc",
          id: "api-reference/rest/get-staffing-gap",
          label: "Get the staffing gap for a path within a committed shift plan",
          className: "api-method get",
        },
        {
          type: "doc",
          id: "api-reference/rest/list-staffing-gaps-for-shift",
          label: "List the staffing gap for every path planned within one committed shift plan (fleet-wide, all-paths view)",
          className: "api-method get",
        },
      ],
    },
    {
      type: "category",
      label: "System",
      link: {
        type: "doc",
        id: "api-reference/rest/system",
      },
      items: [
        {
          type: "doc",
          id: "api-reference/rest/healthz",
          label: "Liveness check",
          className: "api-method get",
        },
        {
          type: "doc",
          id: "api-reference/rest/readyz",
          label: "Readiness check",
          className: "api-method get",
        },
      ],
    },
  ],
};

export default sidebar.apisidebar;
