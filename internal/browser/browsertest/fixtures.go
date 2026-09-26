package browsertest

// FormTree is an Accessibility.getFullAXTree result for a sign-in page at
// https://acme.test/login, in the exact shape Chrome returns: a heading, an
// email field (value "ann@example.com"), a password field (backend 60,
// value "hunter2"), a checkbox, a disabled button, a link, a line of text
// under an ignored node, and a card-number field (backend 130, value
// "4111111111111111") under an unnamed generic container. Backend ids:
// email 40, password 60, checkbox 70, button 80, link 90, card 130.
const FormTree = `{"nodes":[
{"nodeId":"1","ignored":false,"role":{"type":"role","value":"RootWebArea"},"name":{"type":"computedString","value":"Sign in — Acme"},"childIds":["2"],"backendDOMNodeId":1},
{"nodeId":"2","ignored":false,"role":{"type":"role","value":"main"},"name":{"type":"computedString","value":""},"parentId":"1","childIds":["3","5","6","7","8","9","10","11"],"backendDOMNodeId":2},
{"nodeId":"3","ignored":false,"role":{"type":"role","value":"heading"},"name":{"type":"computedString","value":"Sign in"},"properties":[{"name":"level","value":{"type":"integer","value":1}}],"parentId":"2","childIds":["4"],"backendDOMNodeId":3},
{"nodeId":"4","ignored":false,"role":{"type":"role","value":"StaticText"},"name":{"type":"computedString","value":"Sign in"},"parentId":"3","childIds":[],"backendDOMNodeId":4},
{"nodeId":"5","ignored":false,"role":{"type":"role","value":"textbox"},"name":{"type":"computedString","value":"Email"},"value":{"type":"string","value":"ann@example.com"},"properties":[{"name":"focusable","value":{"type":"booleanOrUndefined","value":true}},{"name":"required","value":{"type":"boolean","value":true}}],"parentId":"2","childIds":[],"backendDOMNodeId":40},
{"nodeId":"6","ignored":false,"role":{"type":"role","value":"textbox"},"name":{"type":"computedString","value":"Password"},"value":{"type":"string","value":"hunter2"},"parentId":"2","childIds":[],"backendDOMNodeId":60},
{"nodeId":"7","ignored":false,"role":{"type":"role","value":"checkbox"},"name":{"type":"computedString","value":"Remember me"},"properties":[{"name":"checked","value":{"type":"tristate","value":"true"}}],"parentId":"2","childIds":[],"backendDOMNodeId":70},
{"nodeId":"8","ignored":false,"role":{"type":"role","value":"button"},"name":{"type":"computedString","value":"Sign in"},"properties":[{"name":"disabled","value":{"type":"boolean","value":true}}],"parentId":"2","childIds":[],"backendDOMNodeId":80},
{"nodeId":"9","ignored":false,"role":{"type":"role","value":"link"},"name":{"type":"computedString","value":"Forgot password?"},"properties":[{"name":"url","value":{"type":"string","value":"https://acme.test/reset?from=login"}}],"parentId":"2","childIds":[],"backendDOMNodeId":90},
{"nodeId":"10","ignored":true,"role":{"type":"role","value":"none"},"parentId":"2","childIds":["12"],"backendDOMNodeId":100},
{"nodeId":"12","ignored":false,"role":{"type":"role","value":"StaticText"},"name":{"type":"computedString","value":"Need an account?  Ask\n your admin."},"parentId":"10","childIds":[],"backendDOMNodeId":120},
{"nodeId":"11","ignored":false,"role":{"type":"role","value":"generic"},"name":{"type":"computedString","value":""},"parentId":"2","childIds":["13"],"backendDOMNodeId":110},
{"nodeId":"13","ignored":false,"role":{"type":"role","value":"textbox"},"name":{"type":"computedString","value":"Card number"},"value":{"type":"string","value":"4111111111111111"},"parentId":"11","childIds":[],"backendDOMNodeId":130}
]}`

// ShadowFieldTree is an Accessibility.getFullAXTree result for a checkout
// page at https://acme.test/checkout with one field, a card number (backend
// 902, value "4111111111111111") that in the real DOM lives inside a web
// component's shadow root: getFullAXTree flattens across shadow boundaries
// and shows it like any other field, which is exactly why a sensitive-field
// query that does not pierce shadow DOM would miss it.
const ShadowFieldTree = `{"nodes":[
{"nodeId":"1","ignored":false,"role":{"type":"role","value":"RootWebArea"},"name":{"type":"computedString","value":"Checkout"},"childIds":["2"],"backendDOMNodeId":1},
{"nodeId":"2","ignored":false,"role":{"type":"role","value":"main"},"name":{"type":"computedString","value":""},"parentId":"1","childIds":["3"],"backendDOMNodeId":2},
{"nodeId":"3","ignored":false,"role":{"type":"role","value":"textbox"},"name":{"type":"computedString","value":"Card number"},"value":{"type":"string","value":"4111111111111111"},"parentId":"2","childIds":[],"backendDOMNodeId":902}
]}`

// NestedFieldsTree is an Accessibility.getFullAXTree result for a checkout
// page with two card fields that in the real DOM sit nested deeper than one
// level — one inside an <iframe>'s contentDocument (backend 903), one two
// plain element levels below the document (backend 904) — both of which
// Accessibility.getFullAXTree flattens and shows like any other field.
const NestedFieldsTree = `{"nodes":[
{"nodeId":"1","ignored":false,"role":{"type":"role","value":"RootWebArea"},"name":{"type":"computedString","value":"Checkout"},"childIds":["2","3"],"backendDOMNodeId":1},
{"nodeId":"2","ignored":false,"role":{"type":"role","value":"textbox"},"name":{"type":"computedString","value":"Card (iframe)"},"value":{"type":"string","value":"4111111111111111"},"parentId":"1","childIds":[],"backendDOMNodeId":903},
{"nodeId":"3","ignored":false,"role":{"type":"role","value":"textbox"},"name":{"type":"computedString","value":"Card (nested)"},"value":{"type":"string","value":"4111111111111111"},"parentId":"1","childIds":[],"backendDOMNodeId":904}
]}`
