// Copies the text of the element named by data-copy to the clipboard.
document.addEventListener("click", function (event) {
  var button = event.target.closest("[data-copy]");
  if (!button) return;
  var source = document.getElementById(button.getAttribute("data-copy"));
  if (!source) return;
  var text = source.value || source.textContent;
  var done = function () {
    var label = button.textContent;
    button.textContent = "Copied";
    setTimeout(function () { button.textContent = label; }, 1500);
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, function () { source.select(); });
  } else {
    source.select();
    if (document.execCommand("copy")) done();
  }
});
