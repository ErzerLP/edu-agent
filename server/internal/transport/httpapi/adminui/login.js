const form = document.querySelector(".login-form");
const button = form.querySelector("button[type='submit']");
let message = document.querySelector(".form-message");

function showLoginError(text) {
  if (!message) {
    message = document.createElement("div");
    message.className = "form-message error";
    message.setAttribute("role", "alert");
    form.before(message);
  }
  message.textContent = text;
}

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  button.disabled = true;
  if (message) message.textContent = "";
  try {
    const response = await fetch(form.action, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams(new FormData(form)),
    });
    if (response.ok && response.url.endsWith("/admin/")) {
      window.location.assign("/admin/");
      return;
    }
    if (response.status === 429) {
      showLoginError("登录尝试过于频繁，请稍后再试。");
    } else if (response.status >= 500) {
      showLoginError("暂时无法创建登录会话，请稍后再试。");
    } else {
      showLoginError("用户名或管理密码不正确。");
    }
  } catch {
    showLoginError("无法连接管理服务，请检查服务器状态。");
  } finally {
    button.disabled = false;
  }
});
